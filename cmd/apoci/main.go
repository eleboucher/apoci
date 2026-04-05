package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/apoci/apoci/internal/activitypub"
	"github.com/apoci/apoci/internal/blobstore"
	"github.com/apoci/apoci/internal/config"
	"github.com/apoci/apoci/internal/database"
	"github.com/apoci/apoci/internal/server"
)

var version = "dev"

func main() {
	rootCmd := &cobra.Command{
		Use:     "apoci",
		Short:   "Federated OCI registry with ActivityPub",
		Version: version,
	}

	defaultConfig := "apoci.yaml"
	if env := os.Getenv("APOCI_CONFIG"); env != "" {
		defaultConfig = env
	}

	var configPath string
	rootCmd.PersistentFlags().StringVarP(&configPath, "config", "c", defaultConfig, "config file path")

	rootCmd.AddCommand(serveCmd(&configPath))
	rootCmd.AddCommand(followCmd(&configPath))
	rootCmd.AddCommand(identityCmd(&configPath))

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func serveCmd(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Start the OCI registry server",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(*configPath)
			if err != nil {
				return err
			}

			logger := buildLogger(cfg)

			db, err := openDB(cfg, logger)
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			defer func() { _ = db.Close() }()

			blobs, err := blobstore.New(cfg.DataDir, logger)
			if err != nil {
				return fmt.Errorf("creating blobstore: %w", err)
			}

			identity, err := activitypub.LoadOrCreateIdentity(cfg.Domain, cfg.AccountDomain, cfg.KeyPath, logger)
			if err != nil {
				return fmt.Errorf("loading identity: %w", err)
			}

			srv := server.New(cfg, db, blobs, identity, logger)

			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			err = srv.Start(ctx)
			if err != nil && err != http.ErrServerClosed {
				return fmt.Errorf("server error: %w", err)
			}
			return nil
		},
	}
}

func followCmd(configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "follow",
		Short: "Manage followed peers",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "add <domain|handle|actor-url>",
		Short: "Follow a peer (accepts domain, @user@domain, or full actor URL)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, identity, err := openAll(*configPath)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			input := args[0]
			fmt.Printf("Resolving %s...\n", input)

			targetActorURL, err := activitypub.ResolveFollowTarget(cmd.Context(), input)
			if err != nil {
				return fmt.Errorf("resolving target: %w", err)
			}

			if targetActorURL != input {
				fmt.Printf("Resolved to %s\n", targetActorURL)
			}

			fmt.Printf("Fetching actor %s...\n", targetActorURL)
			actor, err := activitypub.FetchActor(cmd.Context(), targetActorURL)
			if err != nil {
				return fmt.Errorf("fetching actor: %w", err)
			}

			endpoint := activitypub.EndpointFromActorURL(actor.ID)
			err = db.AddFollowRequest(cmd.Context(), actor.ID, actor.PublicKey.PublicKeyPEM, endpoint)
			if err != nil {
				return fmt.Errorf("storing follow request: %w", err)
			}

			followActivity := map[string]any{
				"@context": "https://www.w3.org/ns/activitystreams",
				"id":       identity.ActorURL + "#follow-" + actor.ID,
				"type":     "Follow",
				"actor":    identity.ActorURL,
				"object":   actor.ID,
			}

			activityJSON, err := json.Marshal(followActivity)
			if err != nil {
				return fmt.Errorf("marshaling follow: %w", err)
			}

			fmt.Printf("Sending Follow to %s...\n", actor.Inbox)
			if err := activitypub.DeliverActivity(cmd.Context(), actor.Inbox, activityJSON, identity); err != nil {
				return fmt.Errorf("sending follow: %w", err)
			}

			fmt.Printf("Follow sent to %s.\n", actor.ID)
			fmt.Println("The peer will auto-accept and send an Accept activity back.")

			if err := db.UpsertPeer(cmd.Context(), &database.Peer{
				ActorURL:          actor.ID,
				Endpoint:          activitypub.EndpointFromActorURL(actor.ID),
				ReplicationPolicy: "lazy",
				IsHealthy:         true,
			}); err != nil {
				return fmt.Errorf("recording peer: %w", err)
			}

			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "remove <domain|handle|actor-url>",
		Short: "Unfollow a peer",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, _, err := openAll(*configPath)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			actorURL, err := activitypub.ResolveFollowTarget(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("resolving target: %w", err)
			}

			if err := db.RemoveFollow(cmd.Context(), actorURL); err != nil {
				return err
			}
			fmt.Printf("Unfollowed %s\n", actorURL)
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List followed peers",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, _, err := openAll(*configPath)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			follows, err := db.ListFollows(cmd.Context())
			if err != nil {
				return err
			}

			if len(follows) == 0 {
				fmt.Println("Not following anyone. Use 'apoci follow add <actor-url>' to follow a peer.")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "ACTOR\tENDPOINT\tSINCE")
			for _, f := range follows {
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", f.ActorURL, f.Endpoint, f.ApprovedAt.Format("2006-01-02"))
			}
			_ = w.Flush()
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "pending",
		Short: "List pending follow requests",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, _, err := openAll(*configPath)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			requests, err := db.ListFollowRequests(cmd.Context())
			if err != nil {
				return err
			}

			if len(requests) == 0 {
				fmt.Println("No pending follow requests.")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "ACTOR\tENDPOINT\tREQUESTED")
			for _, r := range requests {
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", r.ActorURL, r.Endpoint, r.RequestedAt.Format("2006-01-02 15:04"))
			}
			_ = w.Flush()
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "accept <domain|handle|actor-url>",
		Short: "Accept a pending follow request and send Accept activity",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, identity, err := openAll(*configPath)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			actorURL, err := activitypub.ResolveFollowTarget(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("resolving target: %w", err)
			}

			fmt.Printf("Accepting follow from %s...\n", actorURL)
			if err := activitypub.SendAccept(cmd.Context(), identity, db, actorURL); err != nil {
				return err
			}
			fmt.Printf("Accepted follow from %s\n", actorURL)
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "reject <domain|handle|actor-url>",
		Short: "Reject a pending follow request and send Reject activity",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, identity, err := openAll(*configPath)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			actorURL, err := activitypub.ResolveFollowTarget(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("resolving target: %w", err)
			}

			if err := activitypub.SendReject(cmd.Context(), identity, db, actorURL); err != nil {
				return err
			}
			fmt.Printf("Rejected follow from %s\n", actorURL)
			return nil
		},
	})

	return cmd
}

func identityCmd(configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "identity",
		Short: "Manage this node's identity",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Show this node's actor URL and public key",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(*configPath)
			if err != nil {
				return err
			}

			identity, err := activitypub.LoadOrCreateIdentity(cfg.Domain, cfg.AccountDomain, cfg.KeyPath, nopLogger())
			if err != nil {
				return err
			}

			pubPEM, err := identity.PublicKeyPEM()
			if err != nil {
				return err
			}

			fmt.Printf("Node:      %s\n", cfg.Name)
			fmt.Printf("Actor URL: %s\n", identity.ActorURL)
			fmt.Printf("Key ID:    %s\n", identity.KeyID())
			fmt.Printf("Domain:    %s\n", identity.Domain)
			if identity.AccountDomain != identity.Domain {
				fmt.Printf("Account:   %s\n", identity.AccountDomain)
			}
			fmt.Printf("Handle:    @registry@%s\n", identity.AccountDomain)
			fmt.Printf("Endpoint:  %s\n", cfg.Endpoint)
			fmt.Printf("Public Key:\n%s", pubPEM)
			return nil
		},
	})

	return cmd
}

func nopLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func openDB(cfg *config.Config, logger *slog.Logger) (*database.DB, error) {
	switch cfg.Database.Driver {
	case "postgres":
		return database.OpenPostgres(cfg.Database.DSN, logger)
	default:
		return database.OpenSQLite(cfg.DataDir, logger)
	}
}

func openAll(configPath string) (*database.DB, *activitypub.Identity, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, nil, err
	}

	logger := nopLogger()

	db, err := openDB(cfg, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("opening database: %w", err)
	}

	identity, err := activitypub.LoadOrCreateIdentity(cfg.Domain, cfg.AccountDomain, cfg.KeyPath, logger)
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("loading identity: %w", err)
	}

	return db, identity, nil
}

func buildLogger(cfg *config.Config) *slog.Logger {
	var level slog.Level
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	if cfg.LogFormat == "json" {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}

	return slog.New(handler)
}
