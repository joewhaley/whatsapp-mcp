package main

import (
	"context"
	"crypto/subtle"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"whatsapp-mcp/localapp"
	"whatsapp-mcp/mcp"

	"github.com/mark3labs/mcp-go/server"
)

// mcpAuthHandler wraps the MCP streamable server with API-key authentication.
// It accepts either an "Authorization: Bearer <key>" header (preferred) or the
// key as the first path segment (/mcp/{apiKey}) for backward compatibility.
// Shared by both the full (whatsmeow) and read-only (local) server modes.
func mcpAuthHandler(streamableServer http.Handler, apiKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/mcp/")
		providedKey := strings.Split(path, "/")[0] // first segment after /mcp/

		authHeader := r.Header.Get("Authorization")
		headerOK := subtle.ConstantTimeCompare([]byte(authHeader), []byte("Bearer "+apiKey)) == 1
		pathOK := subtle.ConstantTimeCompare([]byte(providedKey), []byte(apiKey)) == 1

		var remainingPath string
		switch {
		case headerOK:
			remainingPath = path
		case pathOK:
			remainingPath = strings.TrimPrefix(path, providedKey)
		default:
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte("Unauthorized: Invalid API key"))
			return
		}

		if !strings.HasPrefix(remainingPath, "/") {
			remainingPath = "/" + remainingPath
		}
		r.URL.Path = "/mcp" + remainingPath

		streamableServer.ServeHTTP(w, r)
	}
}

// defaultChatStoragePath returns the standard macOS location of the WhatsApp
// desktop app's message database.
func defaultChatStoragePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Group Containers",
		"group.net.whatsapp.WhatsApp.shared", "ChatStorage.sqlite")
}

// runLocalMode runs the MCP server in read-only "local" mode: it serves the
// native WhatsApp desktop app database directly (read-only, no copy) and does
// not use whatsmeow at all. It blocks until interrupted.
func runLocalMode(apiKey, host, httpPort string, timezone *time.Location) {
	srcPath := os.Getenv("LOCAL_CHATSTORAGE_PATH")
	if srcPath == "" {
		srcPath = defaultChatStoragePath()
	}
	if srcPath == "" {
		log.Fatal("Local mode: could not determine ChatStorage.sqlite path; set LOCAL_CHATSTORAGE_PATH")
	}
	if _, err := os.Stat(srcPath); err != nil {
		log.Fatalf("Local mode: ChatStorage not found at %q: %v", srcPath, err)
	}

	log.Printf("Mode: LOCAL (read-only native WhatsApp app database)")
	log.Printf("Source: %s", srcPath)

	store, err := localapp.OpenServer(localapp.Options{
		ChatStoragePath: srcPath,
		LIDPath:         os.Getenv("LOCAL_LID_PATH"),
		OwnerJID:        os.Getenv("WHATSAPP_OWNER_JID"),
	})
	if err != nil {
		log.Fatalf("Local mode: failed to open native database: %v", err)
	}
	defer store.Close()

	if store.OwnerJID() == "" {
		log.Println("Warning: could not auto-detect your own JID; sent messages may show an empty sender. Set WHATSAPP_OWNER_JID to fix.")
	} else {
		log.Printf("Owner JID: %s", store.OwnerJID())
	}

	mcpServer := mcp.NewReadOnlyMCPServer(store.Messages, store.Media,
		mcp.OwnerInfo{JID: store.OwnerJID(), Name: store.OwnerName()}, timezone)
	log.Println("MCP server initialized (read-only)")

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	streamableServer := server.NewStreamableHTTPServer(
		mcpServer.GetServer(),
		server.WithEndpointPath("/mcp"),
	)
	mux.HandleFunc("/mcp/", mcpAuthHandler(streamableServer, apiKey))

	httpServer := &http.Server{Addr: host + ":" + httpPort, Handler: mux}

	go func() {
		log.Printf("Starting server on http://%s:%s", host, httpPort)
		log.Printf("- Health check: http://%s:%s/health", host, httpPort)
		log.Printf("- MCP endpoint: http://%s:%s/mcp/{API_KEY}", host, httpPort)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	log.Println("WhatsApp MCP running in read-only local mode. Press Ctrl+C to stop.")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	log.Println("\nShutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}
	log.Println("Shutdown complete")
}
