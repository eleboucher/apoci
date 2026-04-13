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
	"git.erwanleboucher.dev/eleboucher/apoci/internal/federation"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/server"
)

var version = "dev"

var (
	headerStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("99"))
	cellStyle    = lipgloss.NewStyle().Padding(0, 1)
	successStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
)

var verbose bool

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
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "enable debug logging")

	rootCmd.AddCommand(serveCmd(&configPath))
	rootCmd.AddCommand(followCmd(&configPath))
	rootCmd.AddCommand(identityCmd(&configPath))
	rootCmd.AddCommand(actorCmd(&configPath))

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

			blobs, err := openBlobStore(cfg, logger)
			if err != nil {
				return fmt.Errorf("creating blobstore: %w", err)
			}

			identity, err := activitypub.LoadOrCreateIdentity(cfg.Endpoint, cfg.Domain, cfg.AccountDomain, cfg.KeyPath, logger)
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

	removeCmd := &cobra.Command{
		Use:   "remove <domain|handle|actor-url>",
		Short: "Unfollow a peer",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			force, _ := cmd.Flags().GetBool("force")
			if c := remoteClient(remote, token); c != nil {
				res, err := c.RemoveFollow(cmd.Context(), args[0], force)
				if err != nil {
					return err
				}
				_, _ = lipgloss.Println(successStyle.Render("Unfollowed " + res["removed"]))
				return nil
			}
			return runFollowRemove(cmd.Context(), *configPath, args[0], force)
		},
	}
	removeCmd.Flags().Bool("force", false, "remove local records even if the peer is unreachable")
	cmd.AddCommand(removeCmd)

	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List followed peers",
		RunE: func(cmd *cobra.Command, args []string) error {
			if c := remoteClient(remote, token); c != nil {
				data, err := c.ListFollows(cmd.Context())
				if err != nil {
					return err
				}
				var follows []database.Actor
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

	outgoingCmd := &cobra.Command{
		Use:   "outgoing",
		Short: "List outgoing follow requests and their status",
		RunE: func(cmd *cobra.Command, args []string) error {
			status, _ := cmd.Flags().GetString("status")
			if c := remoteClient(remote, token); c != nil {
				data, err := c.ListOutgoingFollows(cmd.Context(), status)
				if err != nil {
					return err
				}
				var follows []database.Actor
				if err := json.Unmarshal(data, &follows); err != nil {
					fmt.Println(string(data))
					return nil
				}
				printOutgoingFollows(follows)
				return nil
			}
			return runFollowOutgoing(cmd.Context(), *configPath, status)
		},
	}
	outgoingCmd.Flags().String("status", "", "filter by status (pending, accepted, rejected)")
	cmd.AddCommand(outgoingCmd)

	return cmd
}

func runFollowAdd(ctx context.Context, configPath, input string) error {
	svc, cleanup, err := openFedService(configPath)
	if err != nil {
		return err
	}
	defer cleanup()

	_, _ = lipgloss.Println(dimStyle.Render("Sending Follow to " + input + "..."))
	result, err := svc.AddFollow(ctx, input)
	if err != nil {
		return err
	}
	_, _ = lipgloss.Println(successStyle.Render("Follow sent to " + result.ActorID))
	return nil
}

func runFollowRemove(ctx context.Context, configPath, arg string, force bool) error {
	svc, cleanup, err := openFedService(configPath)
	if err != nil {
		return err
	}
	defer cleanup()

	actorURL, err := svc.RemoveFollow(ctx, arg, force)
	if err != nil {
		return err
	}
	_, _ = lipgloss.Println(successStyle.Render("Unfollowed " + actorURL))
	return nil
}

func followDisplayName(actorURL string, alias *string) string {
	if alias != nil && *alias != "" {
		return *alias
	}
	return actorURL
}

func printFollows(follows []database.Actor) {
	if len(follows) == 0 {
		_, _ = lipgloss.Println(dimStyle.Render("No followers."))
		return
	}
	rows := make([][]string, len(follows))
	for i, f := range follows {
		since := ""
		if f.TheyFollowUsAt != nil {
			since = f.TheyFollowUsAt.Format("2006-01-02")
		}
		rows[i] = []string{followDisplayName(f.ActorURL, f.Alias), f.Endpoint, since}
	}
	printTable([]string{"ACTOR", "ENDPOINT", "SINCE"}, rows)
}

func runFollowList(ctx context.Context, configPath string) error {
	db, _, _, err := openAll(configPath, cliLogger())
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
		rows[i] = []string{followDisplayName(r.ActorURL, r.Alias), r.Endpoint, r.RequestedAt.Format("2006-01-02 15:04")}
	}
	printTable([]string{"ACTOR", "ENDPOINT", "REQUESTED"}, rows)
}

func runFollowPending(ctx context.Context, configPath string) error {
	db, _, _, err := openAll(configPath, cliLogger())
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
	db, identity, cfg, err := openAll(configPath, cliLogger())
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	svc := &federation.Service{
		Fed:      &federation.RealFederator{Identity: identity, Enqueue: nil},
		DB:       db,
		ActorURL: identity.ActorURL,
		Logger:   nopLogger(),
	}

	_, _ = lipgloss.Println(dimStyle.Render("Accepting follow from " + arg + "..."))
	result, err := svc.AcceptFollow(ctx, arg, cfg.Federation.AutoAccept)
	if err != nil {
		return err
	}
	_, _ = lipgloss.Println(successStyle.Render("Accepted follow from " + result.ActorURL))
	if result.FollowedBack {
		_, _ = lipgloss.Println(successStyle.Render("Mutual follow-back sent to " + result.ActorURL))
	}
	return nil
}

func runFollowReject(ctx context.Context, configPath, arg string) error {
	svc, cleanup, err := openFedService(configPath)
	if err != nil {
		return err
	}
	defer cleanup()

	actorURL, err := svc.RejectFollow(ctx, arg)
	if err != nil {
		return err
	}
	_, _ = lipgloss.Println(successStyle.Render("Rejected follow from " + actorURL))
	return nil
}

func runFollowOutgoing(ctx context.Context, configPath, status string) error {
	db, _, _, err := openAll(configPath, cliLogger())
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	var follows []database.Actor
	if status != "" {
		follows, err = db.ListOutgoingFollows(ctx, status)
	} else {
		follows, err = db.ListAllOutgoingFollows(ctx)
	}
	if err != nil {
		return err
	}
	printOutgoingFollows(follows)
	return nil
}

func printOutgoingFollows(follows []database.Actor) {
	if len(follows) == 0 {
		_, _ = lipgloss.Println(dimStyle.Render("No outgoing follow requests."))
		return
	}
	rows := make([][]string, len(follows))
	for i, f := range follows {
		status := "-"
		if f.WeFollowStatus != nil {
			status = *f.WeFollowStatus
		}
		acceptedAt := "-"
		if f.WeFollowAcceptAt != nil {
			acceptedAt = f.WeFollowAcceptAt.Format("2006-01-02 15:04")
		}
		rows[i] = []string{f.ActorURL, status, f.CreatedAt.Format("2006-01-02 15:04"), acceptedAt}
	}
	printTable([]string{"ACTOR", "STATUS", "CREATED", "ACCEPTED"}, rows)
}

func actorCmd(configPath *string) *cobra.Command {
	var remote, token string

	cmd := &cobra.Command{
		Use:   "actor",
		Short: "Inspect known actors",
	}

	cmd.PersistentFlags().StringVar(&remote, "remote", "", "remote instance URL")
	cmd.PersistentFlags().StringVar(&token, "token", "", "registry token for remote auth")

	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List all known actors",
		RunE: func(cmd *cobra.Command, args []string) error {
			if c := remoteClient(remote, token); c != nil {
				data, err := c.ListActors(cmd.Context())
				if err != nil {
					return err
				}
				var actors []database.Actor
				if err := json.Unmarshal(data, &actors); err != nil {
					fmt.Println(string(data))
					return nil
				}
				printActors(actors)
				return nil
			}
			return runActorList(cmd.Context(), *configPath)
		},
	})

	return cmd
}

func runActorList(ctx context.Context, configPath string) error {
	db, _, _, err := openAll(configPath, cliLogger())
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	actors, err := db.ListActors(ctx)
	if err != nil {
		return err
	}
	printActors(actors)
	return nil
}

func printActors(actors []database.Actor) {
	if len(actors) == 0 {
		_, _ = lipgloss.Println(dimStyle.Render("No known actors."))
		return
	}
	rows := make([][]string, len(actors))
	for i, a := range actors {
		healthy := "yes"
		if !a.IsHealthy {
			healthy = dimStyle.Render("no")
		}

		follows := ""
		if a.TheyFollowUs {
			follows += "follower"
		}
		if a.WeFollowThem {
			status := "pending"
			if a.WeFollowStatus != nil {
				status = *a.WeFollowStatus
			}
			if follows != "" {
				follows += ", "
			}
			follows += "following(" + status + ")"
		}
		if follows == "" {
			follows = dimStyle.Render("—")
		}

		lastSeen := "—"
		if a.LastSeenAt != nil {
			lastSeen = a.LastSeenAt.Format("2006-01-02 15:04")
		}

		rows[i] = []string{followDisplayName(a.ActorURL, a.Alias), a.Endpoint, healthy, follows, lastSeen}
	}
	printTable([]string{"ACTOR", "ENDPOINT", "HEALTHY", "RELATIONSHIP", "LAST SEEN"}, rows)
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

			identity, err := activitypub.LoadOrCreateIdentity(
				cfg.Endpoint, cfg.Domain, cfg.AccountDomain, cfg.KeyPath, nopLogger(),
			)
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
		[]string{"Endpoint", endpoint},
		[]string{"Public Key", publicKey},
	)
	printTable(nil, rows)
}

func nopLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func cliLogger() *slog.Logger {
	if verbose {
		opts := &slog.HandlerOptions{Level: slog.LevelDebug}
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return nopLogger()
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

func openBlobStore(cfg *config.Config, logger *slog.Logger) (blobstore.BlobStore, error) {
	switch cfg.Storage.Type {
	case "s3":
		return blobstore.NewS3(blobstore.S3Config{
			Bucket:         cfg.Storage.S3.Bucket,
			Region:         cfg.Storage.S3.Region,
			Endpoint:       cfg.Storage.S3.Endpoint,
			AccessKey:      cfg.Storage.S3.AccessKey,
			SecretKey:      cfg.Storage.S3.SecretKey,
			Prefix:         cfg.Storage.S3.Prefix,
			ForcePathStyle: cfg.Storage.S3.ForcePathStyle,
			TempDir:        cfg.Storage.S3.TempDir,
		}, logger)
	case "local":
		return blobstore.New(cfg.DataDir, logger)
	default:
		return nil, fmt.Errorf("unknown storage type: %s", cfg.Storage.Type)
	}
}

func openDB(cfg *config.Config, logger *slog.Logger) (*database.DB, error) {
	switch cfg.Database.Driver {
	case "postgres":
		return database.OpenPostgres(cfg.Database.DSN, cfg.Database.MaxOpenConns, cfg.Database.MaxIdleConns, logger)
	default:
		return database.OpenSQLite(cfg.DataDir, cfg.Database.MaxOpenConns, cfg.Database.MaxIdleConns, logger)
	}
}

func openAll(configPath string, logger *slog.Logger) (*database.DB, *activitypub.Identity, *config.Config, error) {
	logger.Debug("loading config", "path", configPath)
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, nil, nil, err
	}

	logger.Debug("opening database", "driver", cfg.Database.Driver)
	db, err := openDB(cfg, logger)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("opening database: %w", err)
	}

	logger.Debug("loading identity", "endpoint", cfg.Endpoint, "domain", cfg.Domain)
	identity, err := activitypub.LoadOrCreateIdentity(cfg.Endpoint, cfg.Domain, cfg.AccountDomain, cfg.KeyPath, logger)
	if err != nil {
		_ = db.Close()
		return nil, nil, nil, fmt.Errorf("loading identity: %w", err)
	}

	return db, identity, cfg, nil
}

func openFedService(configPath string) (*federation.Service, func(), error) {
	logger := cliLogger()
	db, identity, _, err := openAll(configPath, logger)
	if err != nil {
		return nil, nil, err
	}
	logger.Debug("federation service ready", "actorURL", identity.ActorURL)
	svc := &federation.Service{
		Fed:      &federation.RealFederator{Identity: identity, Enqueue: nil},
		DB:       db,
		ActorURL: identity.ActorURL,
		Logger:   logger,
	}
	return svc, func() { _ = db.Close() }, nil
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
