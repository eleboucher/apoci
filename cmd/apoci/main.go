package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/spf13/cobra"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/activitypub"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/admin"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/config"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/server"
)

var version = "dev"

var (
	headerStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("99"))
	cellStyle    = lipgloss.NewStyle().Padding(0, 1)
	successStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
)

func main() {
	rootCmd := &cobra.Command{
		Use:     "apoci",
		Short:   "Federated OCI registry with ActivityPub",
		Version: version,
	}

	defaultConfig := "config/apoci.yaml"
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

			srv, err := server.New(cfg, db, blobs, identity, version, logger)
			if err != nil {
				return fmt.Errorf("creating server: %w", err)
			}

			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			err = srv.Start(ctx)
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("server error: %w", err)
			}
			return nil
		},
	}
}

func followCmd(configPath *string) *cobra.Command {
	var remote, token string

	cmd := &cobra.Command{
		Use:   "follow",
		Short: "Manage followed peers",
	}

	cmd.PersistentFlags().StringVar(&remote, "remote", "", "remote instance URL (e.g. https://registry.example.com)")
	cmd.PersistentFlags().StringVar(&token, "token", "", "registry token for remote auth")

	cmd.AddCommand(&cobra.Command{
		Use:   "add <domain|handle|actor-url>",
		Short: "Follow a peer (accepts domain, @user@domain, or full actor URL)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if c := remoteClient(remote, token); c != nil {
				res, err := c.AddFollow(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				_, _ = lipgloss.Println(successStyle.Render("Follow sent to " + res["followed"]))
				return nil
			}
			return runFollowAdd(cmd.Context(), *configPath, args[0])
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "remove <domain|handle|actor-url>",
		Short: "Unfollow a peer",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if c := remoteClient(remote, token); c != nil {
				res, err := c.RemoveFollow(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				_, _ = lipgloss.Println(successStyle.Render("Unfollowed " + res["removed"]))
				return nil
			}
			return runFollowRemove(cmd.Context(), *configPath, args[0])
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List followed peers",
		RunE: func(cmd *cobra.Command, args []string) error {
			if c := remoteClient(remote, token); c != nil {
				data, err := c.ListFollows(cmd.Context())
				if err != nil {
					return err
				}
				var follows []database.Follow
				if err := json.Unmarshal(data, &follows); err != nil {
					fmt.Println(string(data))
					return nil
				}
				printFollows(follows)
				return nil
			}
			return runFollowList(cmd.Context(), *configPath)
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "pending",
		Short: "List pending follow requests",
		RunE: func(cmd *cobra.Command, args []string) error {
			if c := remoteClient(remote, token); c != nil {
				data, err := c.ListPending(cmd.Context())
				if err != nil {
					return err
				}
				var requests []database.FollowRequest
				if err := json.Unmarshal(data, &requests); err != nil {
					fmt.Println(string(data))
					return nil
				}
				printFollowRequests(requests)
				return nil
			}
			return runFollowPending(cmd.Context(), *configPath)
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "accept <domain|handle|actor-url>",
		Short: "Accept a pending follow request",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if c := remoteClient(remote, token); c != nil {
				res, err := c.AcceptFollow(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				_, _ = lipgloss.Println(successStyle.Render("Accepted follow from " + res["accepted"]))
				return nil
			}
			return runFollowAccept(cmd.Context(), *configPath, args[0])
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "reject <domain|handle|actor-url>",
		Short: "Reject a pending follow request",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if c := remoteClient(remote, token); c != nil {
				res, err := c.RejectFollow(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				_, _ = lipgloss.Println(successStyle.Render("Rejected follow from " + res["rejected"]))
				return nil
			}
			return runFollowReject(cmd.Context(), *configPath, args[0])
		},
	})

	return cmd
}

func runFollowAdd(ctx context.Context, configPath, input string) error {
	db, identity, err := openAll(configPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	_, _ = lipgloss.Println(dimStyle.Render("Resolving " + input + "..."))
	targetActorURL, err := activitypub.ResolveFollowTarget(ctx, input)
	if err != nil {
		return fmt.Errorf("resolving target: %w", err)
	}
	if targetActorURL != input {
		_, _ = lipgloss.Println(dimStyle.Render("Resolved to " + targetActorURL))
	}

	_, _ = lipgloss.Println(dimStyle.Render("Fetching actor " + targetActorURL + "..."))
	actor, err := activitypub.FetchActor(ctx, targetActorURL)
	if err != nil {
		return fmt.Errorf("fetching actor: %w", err)
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

	_, _ = lipgloss.Println(dimStyle.Render("Sending Follow to " + actor.Inbox + "..."))
	if err := activitypub.DeliverActivity(ctx, actor.Inbox, activityJSON, identity); err != nil {
		return fmt.Errorf("sending follow: %w", err)
	}
	_, _ = lipgloss.Println(successStyle.Render("Follow sent to " + actor.ID))

	// Record the outgoing follow so handleAccept can match the Accept(Follow)
	// back to this request when the peer responds.
	if err := db.AddOutgoingFollow(ctx, actor.ID); err != nil {
		return fmt.Errorf("storing outgoing follow: %w", err)
	}

	if err := db.UpsertPeer(ctx, &database.Peer{
		ActorURL:          actor.ID,
		Endpoint:          activitypub.EndpointFromActorURL(actor.ID),
		ReplicationPolicy: "lazy",
		IsHealthy:         true,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to upsert peer record: %v\n", err)
	}
	return nil
}

func runFollowRemove(ctx context.Context, configPath, arg string) error {
	db, identity, err := openAll(configPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	actorURL, err := activitypub.ResolveFollowTarget(ctx, arg)
	if err != nil {
		return fmt.Errorf("resolving target: %w", err)
	}

	if err := activitypub.SendUndo(ctx, identity, actorURL, nil); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}

	if err := db.RemoveFollow(ctx, actorURL); err != nil {
		fmt.Fprintf(os.Stderr, "warning: removing follow: %v\n", err)
	}
	if err := db.RemoveOutgoingFollow(ctx, actorURL); err != nil {
		fmt.Fprintf(os.Stderr, "warning: removing outgoing follow: %v\n", err)
	}
	_, _ = lipgloss.Println(successStyle.Render("Unfollowed " + actorURL))
	return nil
}

func printFollows(follows []database.Follow) {
	if len(follows) == 0 {
		_, _ = lipgloss.Println(dimStyle.Render("Not following anyone."))
		return
	}
	rows := make([][]string, len(follows))
	for i, f := range follows {
		rows[i] = []string{f.ActorURL, f.Endpoint, f.ApprovedAt.Format("2006-01-02")}
	}
	printTable([]string{"ACTOR", "ENDPOINT", "SINCE"}, rows)
}

func runFollowList(ctx context.Context, configPath string) error {
	db, _, err := openAll(configPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	follows, err := db.ListFollows(ctx)
	if err != nil {
		return err
	}
	printFollows(follows)
	return nil
}

func printFollowRequests(requests []database.FollowRequest) {
	if len(requests) == 0 {
		_, _ = lipgloss.Println(dimStyle.Render("No pending follow requests."))
		return
	}
	rows := make([][]string, len(requests))
	for i, r := range requests {
		rows[i] = []string{r.ActorURL, r.Endpoint, r.RequestedAt.Format("2006-01-02 15:04")}
	}
	printTable([]string{"ACTOR", "ENDPOINT", "REQUESTED"}, rows)
}

func runFollowPending(ctx context.Context, configPath string) error {
	db, _, err := openAll(configPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	requests, err := db.ListFollowRequests(ctx)
	if err != nil {
		return err
	}
	printFollowRequests(requests)
	return nil
}

func runFollowAccept(ctx context.Context, configPath, arg string) error {
	db, identity, err := openAll(configPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	actorURL, err := activitypub.ResolveFollowTarget(ctx, arg)
	if err != nil {
		return fmt.Errorf("resolving target: %w", err)
	}
	_, _ = lipgloss.Println(dimStyle.Render("Accepting follow from " + actorURL + "..."))
	if err := activitypub.SendAccept(ctx, identity, db, actorURL, nil); err != nil {
		return fmt.Errorf("sending accept: %w", err)
	}
	_, _ = lipgloss.Println(successStyle.Render("Accepted follow from " + actorURL))
	return nil
}

func runFollowReject(ctx context.Context, configPath, arg string) error {
	db, identity, err := openAll(configPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	actorURL, err := activitypub.ResolveFollowTarget(ctx, arg)
	if err != nil {
		return fmt.Errorf("resolving target: %w", err)
	}
	if err := activitypub.SendReject(ctx, identity, db, actorURL, nil); err != nil {
		return err
	}
	_, _ = lipgloss.Println(successStyle.Render("Rejected follow from " + actorURL))
	return nil
}

func identityCmd(configPath *string) *cobra.Command {
	var remote, token string

	cmd := &cobra.Command{
		Use:   "identity",
		Short: "Manage this node's identity",
	}

	cmd.PersistentFlags().StringVar(&remote, "remote", "", "remote instance URL")
	cmd.PersistentFlags().StringVar(&token, "token", "", "registry token for remote auth")

	cmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Show this node's actor URL and public key",
		RunE: func(cmd *cobra.Command, args []string) error {
			if c := remoteClient(remote, token); c != nil {
				info, err := c.GetIdentity(cmd.Context())
				if err != nil {
					return err
				}
				printIdentity(info["name"], info["actorURL"], info["keyID"],
					info["domain"], info["accountDomain"], info["endpoint"], info["publicKey"])
				return nil
			}

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

			printIdentity(cfg.Name, identity.ActorURL, identity.KeyID(),
				identity.Domain, identity.AccountDomain, cfg.Endpoint, pubPEM)
			return nil
		},
	})

	return cmd
}

func printIdentity(name, actorURL, keyID, domain, accountDomain, endpoint, publicKey string) {
	rows := [][]string{
		{"Node", name},
		{"Actor URL", actorURL},
		{"Key ID", keyID},
		{"Domain", domain},
	}
	if accountDomain != domain {
		rows = append(rows, []string{"Account", accountDomain})
	}
	rows = append(rows,
		[]string{"Handle", "@registry@" + accountDomain},
		[]string{"Endpoint", endpoint},
		[]string{"Public Key", publicKey},
	)
	printTable(nil, rows)
}

func nopLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func remoteClient(remote, token string) *admin.Client {
	if remote == "" {
		remote = os.Getenv("APOCI_REMOTE_URL")
	}
	if token == "" {
		token = os.Getenv("APOCI_ADMIN_TOKEN")
	}
	if remote == "" {
		if token != "" {
			fmt.Fprintln(os.Stderr, "warning: --token has no effect without --remote")
		}
		return nil
	}
	return admin.NewClient(remote, token)
}

func printTable(headers []string, rows [][]string) {
	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("238"))).
		StyleFunc(func(row, col int) lipgloss.Style {
			if row == table.HeaderRow {
				return headerStyle
			}
			return cellStyle
		}).
		Rows(rows...)
	if len(headers) > 0 {
		t.Headers(headers...)
	}
	_, _ = lipgloss.Println(t)
}

func openDB(cfg *config.Config, logger *slog.Logger) (*database.DB, error) {
	switch cfg.Database.Driver {
	case "postgres":
		return database.OpenPostgres(cfg.Database.DSN, cfg.Database.MaxOpenConns, cfg.Database.MaxIdleConns, logger)
	default:
		return database.OpenSQLite(cfg.DataDir, cfg.Database.MaxOpenConns, cfg.Database.MaxIdleConns, logger)
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

var logLevels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

func buildLogger(cfg *config.Config) *slog.Logger {
	level, ok := logLevels[cfg.LogLevel]
	if !ok {
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
