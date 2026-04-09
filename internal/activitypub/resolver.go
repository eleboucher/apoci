package activitypub

import (
	"context"
	"fmt"
	"log/slog"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/oci"
)

type ResolverRepository interface {
	FindPeersWithBlob(ctx context.Context, digest string) ([]database.PeerBlob, error)
}

type APResolver struct {
	db     ResolverRepository
	logger *slog.Logger
}

func NewAPResolver(db ResolverRepository, logger *slog.Logger) *APResolver {
	return &APResolver{db: db, logger: logger}
}

func (r *APResolver) FindBlobPeers(ctx context.Context, digest string) ([]oci.BlobPeer, error) {
	peerBlobs, err := r.db.FindPeersWithBlob(ctx, digest)
	if err != nil {
		return nil, fmt.Errorf("finding peers with blob: %w", err)
	}

	var results []oci.BlobPeer
	for _, pb := range peerBlobs {
		results = append(results, oci.BlobPeer{PeerEndpoint: pb.PeerEndpoint})
	}

	return results, nil
}
