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
	"path/filepath"
	"strings"
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
	"git.erwanleboucher.dev/eleboucher/apoci/internal/notify"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/oci"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/peering"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/server"
)

const cmdList = "list"

var version = "dev"

var (
	headerStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("99"))
	cellStyle    = lipgloss.NewStyle().Padding(0, 1)
	successStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
)

var verbose bool

// configMustExist is true when the config path was set explicitly (-c or
// APOCI_CONFIG), so a missing file errors instead of falling back to defaults.
var configMustExist bool

const (
	colActor    = "ACTOR"
	colEndpoint = "ENDPOINT"
)

func main() {
	rootCmd := &cobra.Command{
		Use:     "apoci",
		Short:   "Federated OCI registry with ActivityPub",
		Version: version,
	}

	defaultConfig := "configs/apoci.yaml"
	envConfig := os.Getenv("APOCI_CONFIG") != ""
	if env := os.Getenv("APOCI_CONFIG"); env != "" {
		defaultConfig = env
	}

	var configPath string
	rootCmd.PersistentFlags().StringVarP(&configPath, "config", "c", defaultConfig, "config file path")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "enable debug logging")

	rootCmd.PersistentPreRun = func(cmd *cobra.Command, _ []string) {
		configMustExist = envConfig || cmd.Flags().Changed("config")
	}

	rootCmd.AddCommand(serveCmd(&configPath))
	rootCmd.AddCommand(followCmd(&configPath))
	rootCmd.AddCommand(identityCmd(&configPath))
	rootCmd.AddCommand(actorCmd(&configPath))
	rootCmd.AddCommand(imagesCmd(&configPath))
	rootCmd.AddCommand(mirrorCmd(&configPath))
	rootCmd.AddCommand(uploadsCmd(&configPath))
	rootCmd.AddCommand(gcCmd(&configPath))

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func serveCmd(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Start the OCI registry server",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(*configPath, configMustExist)
			if err != nil {
				return err
			}

			logger := buildLogger(cfg)
			warnGeneratedTokens(logger, cfg)

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
		Use:   cmdList,
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

	filterCmd := &cobra.Command{
		Use:   "filter <domain|handle|actor-url>",
		Short: "Set tag-glob filter for an inbound follower",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tag, _ := cmd.Flags().GetString("tag")
			clear, _ := cmd.Flags().GetBool("clear")
			var globs []string
			if !clear && tag != "" {
				for g := range strings.SplitSeq(tag, ",") {
					if s := strings.TrimSpace(g); s != "" {
						globs = append(globs, s)
					}
				}
			}
			if c := remoteClient(remote, token); c != nil {
				if _, err := c.UpdateFollowFilter(cmd.Context(), args[0], globs); err != nil {
					return err
				}
				if clear || len(globs) == 0 {
					_, _ = lipgloss.Println(successStyle.Render("Filter cleared for " + args[0]))
				} else {
					_, _ = lipgloss.Println(successStyle.Render("Filter set for " + args[0] + " (" + strings.Join(globs, ", ") + ")"))
				}
				return nil
			}
			return runFollowFilter(cmd.Context(), *configPath, args[0], globs)
		},
	}
	filterCmd.Flags().String("tag", "", "comma-separated glob list (e.g. \"latest,v*\")")
	filterCmd.Flags().Bool("clear", false, "clear the filter (deliver everything)")
	cmd.AddCommand(filterCmd)

	return cmd
}

func runFollowFilter(ctx context.Context, configPath, target string, globs []string) error {
	db, _, _, err := openAll(configPath, cliLogger())
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if err := db.UpdateFollowFilter(ctx, target, globs); err != nil {
		return err
	}
	if len(globs) == 0 {
		_, _ = lipgloss.Println(successStyle.Render("Filter cleared for " + target))
	} else {
		_, _ = lipgloss.Println(successStyle.Render("Filter set for " + target + " (" + strings.Join(globs, ", ") + ")"))
	}
	return nil
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
	printTable([]string{colActor, colEndpoint, "SINCE"}, rows)
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
	printTable([]string{colActor, colEndpoint, "REQUESTED"}, rows)
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
	printTable([]string{colActor, "STATUS", "CREATED", "ACCEPTED"}, rows)
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
		Use:   cmdList,
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
	printTable([]string{colActor, colEndpoint, "HEALTHY", "RELATIONSHIP", "LAST SEEN"}, rows)
}

func imagesCmd(configPath *string) *cobra.Command {
	var remote, token string

	cmd := &cobra.Command{
		Use:   "images",
		Short: "List locally hosted images",
	}

	cmd.PersistentFlags().StringVar(&remote, "remote", "", "remote instance URL")
	cmd.PersistentFlags().StringVar(&token, "token", "", "registry token for remote auth")

	cmd.AddCommand(&cobra.Command{
		Use:   cmdList,
		Short: "List all locally hosted images and their size",
		RunE: func(cmd *cobra.Command, args []string) error {
			if c := remoteClient(remote, token); c != nil {
				images, err := c.ListImages(cmd.Context())
				if err != nil {
					return err
				}
				printImages(images)
				return nil
			}
			return runImageList(cmd.Context(), *configPath)
		},
	})

	return cmd
}

func mirrorCmd(configPath *string) *cobra.Command {
	var remote, token string

	cmd := &cobra.Command{
		Use:   "mirror",
		Short: "Manage upstream image mirrors",
	}

	cmd.PersistentFlags().StringVar(&remote, "remote", "", "remote instance URL")
	cmd.PersistentFlags().StringVar(&token, "token", "", "registry token for remote auth")

	evictCmd := &cobra.Command{
		Use:   "evict <repo>",
		Short: "Drop a locally-mirrored upstream repository (does not affect the upstream)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			digest, _ := cmd.Flags().GetString("digest")
			repo := args[0]
			if c := remoteClient(remote, token); c != nil {
				res, err := c.EvictMirror(cmd.Context(), repo, digest)
				if err != nil {
					return err
				}
				msg := "Evicted mirror " + res["evicted"]
				if d := res["digest"]; d != "" {
					msg += "@" + d
				}
				_, _ = lipgloss.Println(successStyle.Render(msg))
				return nil
			}
			return runMirrorEvict(cmd.Context(), *configPath, repo, digest)
		},
	}
	evictCmd.Flags().String("digest", "", "evict only the manifest with this digest (sha256:...)")
	cmd.AddCommand(evictCmd)

	return cmd
}

func uploadsCmd(configPath *string) *cobra.Command {
	var remote, token string

	cmd := &cobra.Command{
		Use:   "uploads",
		Short: "Manage in-progress chunked upload sessions",
	}

	cmd.PersistentFlags().StringVar(&remote, "remote", "", "remote instance URL")
	cmd.PersistentFlags().StringVar(&token, "token", "", "registry token for remote auth")

	cmd.AddCommand(&cobra.Command{
		Use:   cmdList,
		Short: "List in-progress upload sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			if c := remoteClient(remote, token); c != nil {
				uploads, err := c.ListUploads(cmd.Context())
				if err != nil {
					return err
				}
				printUploads(uploads)
				return nil
			}
			return runUploadsList(cmd.Context(), *configPath)
		},
	})

	pruneCmd := &cobra.Command{
		Use:   "prune [repo]",
		Short: "Force-clear upload sessions to recover a repository wedged by a partial push",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			all, _ := cmd.Flags().GetBool("all")
			var repo string
			if len(args) == 1 {
				repo = args[0]
			}
			if repo == "" && !all {
				return errors.New("specify a repository, or pass --all to clear every upload session")
			}
			if repo != "" && all {
				return errors.New("pass either a repository or --all, not both")
			}

			if c := remoteClient(remote, token); c != nil {
				res, err := c.PurgeUploads(cmd.Context(), repo)
				if err != nil {
					return err
				}
				_, _ = lipgloss.Println(successStyle.Render(fmt.Sprintf("Purged %v upload session(s)", res["purged"])))
				return nil
			}
			n, err := runUploadsPrune(cmd.Context(), *configPath, repo)
			if err != nil {
				return err
			}
			_, _ = lipgloss.Println(successStyle.Render(fmt.Sprintf("Purged %d upload session(s)", n)))
			return nil
		},
	}
	pruneCmd.Flags().Bool("all", false, "clear every upload session across all repositories")
	cmd.AddCommand(pruneCmd)

	return cmd
}

func runUploadsList(ctx context.Context, configPath string) error {
	logger := cliLogger()
	db, _, _, err := openAll(configPath, logger)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	sessions, err := db.ListUploadSessions(ctx)
	if err != nil {
		return err
	}
	entries := make([]admin.UploadSessionEntry, len(sessions))
	for i, s := range sessions {
		entries[i] = admin.UploadSessionEntry{
			UUID:          s.UUID,
			RepositoryID:  s.RepositoryID,
			BytesReceived: s.BytesReceived,
			CreatedAt:     s.CreatedAt,
			ExpiresAt:     s.ExpiresAt,
		}
	}
	printUploads(entries)
	return nil
}

func runUploadsPrune(ctx context.Context, configPath, repo string) (int, error) {
	logger := cliLogger()
	db, identity, cfg, err := openAll(configPath, logger)
	if err != nil {
		return 0, err
	}
	defer func() { _ = db.Close() }()
	blobs, err := openBlobStore(cfg, logger)
	if err != nil {
		return 0, fmt.Errorf("opening blobstore: %w", err)
	}
	reg, err := oci.NewRegistry(
		db, blobs, identity.ActorURL, cfg.AccountDomain,
		cfg.Limits.MaxManifestSize, cfg.Limits.MaxBlobSize, logger,
	)
	if err != nil {
		return 0, err
	}
	// Mirror server.go's staging-dir choice so we sweep the same files.
	uploadDir := os.TempDir()
	switch {
	case cfg.BlobDiskPath() != "":
		uploadDir = filepath.Join(cfg.DataDir, "uploads")
	case cfg.Storage.S3.TempDir != "":
		uploadDir = cfg.Storage.S3.TempDir
	}
	if err := reg.SetUploadDir(uploadDir); err != nil {
		return 0, err
	}
	return reg.PurgeUploadSessions(ctx, repo)
}

func printUploads(uploads []admin.UploadSessionEntry) {
	if len(uploads) == 0 {
		_, _ = lipgloss.Println(dimStyle.Render("No in-progress upload sessions"))
		return
	}
	t := table.New().
		Border(lipgloss.NormalBorder()).
		Headers("UUID", "REPO ID", "BYTES", "EXPIRES")
	for _, u := range uploads {
		t.Row(
			u.UUID,
			fmt.Sprintf("%d", u.RepositoryID),
			fmt.Sprintf("%d", u.BytesReceived),
			u.ExpiresAt.Format("2006-01-02 15:04:05"),
		)
	}
	_, _ = lipgloss.Println(t)
}

func gcCmd(configPath *string) *cobra.Command {
	var remote, token string

	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Manage the garbage collector",
	}

	cmd.PersistentFlags().StringVar(&remote, "remote", "", "remote instance URL")
	cmd.PersistentFlags().StringVar(&token, "token", "", "registry token for remote auth")

	cmd.AddCommand(&cobra.Command{
		Use:   "run",
		Short: "Run a GC cycle now (synchronous)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if c := remoteClient(remote, token); c != nil {
				_, _ = lipgloss.Println(dimStyle.Render("Triggering GC..."))
				if _, err := c.RunGC(cmd.Context()); err != nil {
					return err
				}
				_, _ = lipgloss.Println(successStyle.Render("GC cycle complete"))
				return nil
			}
			return runGCRun(cmd.Context(), *configPath)
		},
	})

	return cmd
}

func runGCRun(ctx context.Context, configPath string) error {
	logger := cliLogger()
	db, identity, cfg, err := openAll(configPath, logger)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	blobs, err := openBlobStore(cfg, logger)
	if err != nil {
		return fmt.Errorf("opening blobstore: %w", err)
	}

	notifier := notify.New(cfg.Name, nil, nil, logger)
	perRepo := make(map[string]peering.RetentionPolicy, len(cfg.GC.Retention.PerRepo))
	for _, r := range cfg.GC.Retention.PerRepo {
		perRepo[r.Repo] = peering.RetentionPolicy{
			KeepLastN:   r.KeepLastN,
			MaxAge:      r.MaxAge,
			PinnedGlobs: r.PinnedGlobs,
		}
	}
	gc := peering.NewGarbageCollector(peering.GCConfig{
		Interval:               cfg.GC.Interval,
		StalePeerBlobAge:       cfg.GC.StalePeerBlobAge,
		OrphanBatchSize:        cfg.GC.OrphanBatchSize,
		BlobGCGracePeriod:      cfg.GC.BlobGCGracePeriod,
		UntaggedManifestAge:    cfg.GC.UntaggedManifestAge,
		UntaggedBatchSize:      cfg.GC.UntaggedBatchSize,
		RetentionTagsPerCycle:  cfg.GC.RetentionTagsPerCycle,
		DiskUsageThreshold:     cfg.GC.DiskUsageThreshold,
		DiskUsageCheckInterval: cfg.GC.DiskUsageCheckInterval,
		DiskUsagePath:          cfg.BlobDiskPath(),
		RetentionDefaults: peering.RetentionPolicy{
			KeepLastN:   cfg.GC.Retention.KeepLastN,
			MaxAge:      cfg.GC.Retention.MaxAge,
			PinnedGlobs: cfg.GC.Retention.PinnedGlobs,
		},
		RetentionPerRepo: perRepo,
		LocalActor:       identity.ActorURL,
	}, db, blobs, notifier, logger)

	_, _ = lipgloss.Println(dimStyle.Render("Running GC..."))
	gc.RunOnce(ctx)
	_, _ = lipgloss.Println(successStyle.Render("GC cycle complete"))
	return nil
}

func runMirrorEvict(ctx context.Context, configPath, repo, digest string) error {
	db, identity, _, err := openAll(configPath, cliLogger())
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	repoObj, err := db.GetRepository(ctx, repo)
	if err != nil {
		return fmt.Errorf("looking up repository: %w", err)
	}
	if repoObj == nil {
		return fmt.Errorf("repository %q not found", repo)
	}
	if repoObj.OwnerID == identity.ActorURL {
		return fmt.Errorf("repository %q is locally owned; use the OCI delete API to remove it", repo)
	}

	if digest != "" {
		if err := db.DeleteManifest(ctx, repoObj.ID, digest); err != nil {
			return fmt.Errorf("evicting manifest: %w", err)
		}
		_, _ = lipgloss.Println(successStyle.Render("Evicted mirror " + repo + "@" + digest))
		return nil
	}

	if err := db.DeleteRepository(ctx, repoObj.ID); err != nil {
		return fmt.Errorf("evicting repository: %w", err)
	}
	_, _ = lipgloss.Println(successStyle.Render("Evicted mirror " + repo))
	return nil
}

func runImageList(ctx context.Context, configPath string) error {
	db, _, _, err := openAll(configPath, cliLogger())
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	repos, err := db.ListLocallyHostedRepos(ctx)
	if err != nil {
		return err
	}
	entries := make([]admin.ImageEntry, len(repos))
	for i, r := range repos {
		entries[i] = admin.ImageEntry{
			Name:      r.Name,
			Tags:      r.Tags,
			SizeBytes: r.SizeBytes,
			UpdatedAt: r.UpdatedAt,
		}
	}
	printImages(entries)
	return nil
}

func printImages(images []admin.ImageEntry) {
	if len(images) == 0 {
		_, _ = lipgloss.Println(dimStyle.Render("No locally hosted images."))
		return
	}
	rows := make([][]string, len(images))
	for i, img := range images {
		tags := "—"
		if len(img.Tags) > 0 {
			tags = strings.Join(img.Tags, ", ")
		}
		rows[i] = []string{img.Name, formatBytes(img.SizeBytes), tags, img.UpdatedAt.Format("2006-01-02 15:04")}
	}
	printTable([]string{"NAME", "SIZE", "TAGS", "UPDATED"}, rows)
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
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

			cfg, err := config.Load(*configPath, configMustExist)
			if err != nil {
				return err
			}
			warnGeneratedTokens(buildLogger(cfg), cfg)

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
	case config.StorageTypeS3:
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
	case config.StorageTypeLocal:
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
	cfg, err := config.Load(configPath, configMustExist)
	if err != nil {
		return nil, nil, nil, err
	}
	warnGeneratedTokens(logger, cfg)

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

// warnGeneratedTokens logs a startup warning for each token config.Load
// auto-generated because none was supplied. The token value is never logged.
func warnGeneratedTokens(logger *slog.Logger, cfg *config.Config) {
	for _, path := range cfg.GeneratedTokenPaths {
		logger.Warn("no token supplied; auto-generated a random one and wrote it to disk", "path", path)
	}
}
