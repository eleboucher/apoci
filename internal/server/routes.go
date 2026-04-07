package server

import (
	"encoding/json"
	"net/http"
)

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	mux.Handle("/v2/", registryAuthMiddleware(s.cfg.RegistryToken)(s.ociHandler))

	mux.Handle("GET /.well-known/webfinger", s.webfingerHandler)
	mux.Handle("GET /.well-known/nodeinfo", http.HandlerFunc(s.nodeinfoHandler.ServeWellKnown))
	mux.Handle("GET /ap/nodeinfo/2.1", http.HandlerFunc(s.nodeinfoHandler.ServeNodeInfo))
	mux.Handle("GET /ap/actor", s.actorHandler)
	mux.Handle("POST /ap/inbox", rateLimitMiddleware(s.inboxLimiter)(s.inboxHandler))
	mux.Handle("GET /ap/outbox", s.outboxHandler)
	mux.Handle("GET /ap/followers", s.followersHandler)
	mux.Handle("GET /ap/following", s.followingHandler)

	mux.Handle("/api/admin/", http.StripPrefix("/api/admin", s.adminRouter()))

	var handler http.Handler = mux
	handler = loggingMiddleware(s.logger)(handler)
	handler = requestIDMiddleware(handler)
	handler = recoveryMiddleware(s.logger)(handler)

	return handler
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if err := s.db.Ping(); err != nil {
		s.logger.Warn("readyz check failed", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "not ready"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
}
