package mcp

import (
	"log"
	"time"

	"whatsapp-mcp/storage"
	"whatsapp-mcp/whatsapp"

	"github.com/mark3labs/mcp-go/server"
)

// OwnerInfo holds the account owner's identity for read-only (local) mode,
// where there is no live WhatsApp client to query for profile information.
type OwnerInfo struct {
	JID  string
	Name string
}

// MCPServer represents an MCP server instance for WhatsApp integration.
type MCPServer struct {
	server     *server.MCPServer
	wa         *whatsapp.Client // nil in read-only (local) mode
	store      *storage.MessageStore
	mediaStore *storage.MediaStore
	log        *log.Logger
	timezone   *time.Location
	readOnly   bool      // true when serving the native app DB read-only
	owner      OwnerInfo // owner identity for read-only mode
}

const fullModeInstructions = `WhatsApp integration for messaging operations.

Key workflow: find_chat → get_chat_messages or send_message
Always get chat_jid from find_chat before other operations.
JIDs are WhatsApp identifiers (e.g., 5511999999999@s.whatsapp.net).

Use prompts for common workflows or resources for detailed guides.`

const readOnlyModeInstructions = `WhatsApp integration (READ-ONLY local mode).

This server reads the WhatsApp desktop app's local database directly. It can
browse and search your full message history but CANNOT send messages or fetch
new history from WhatsApp's servers (send_message, load_more_messages and
sync_history are unavailable in this mode).

Key workflow: find_chat → get_chat_messages
Always get chat_jid from find_chat before other operations.
JIDs are WhatsApp identifiers (e.g., 5511999999999@s.whatsapp.net).

Use prompts for common workflows or resources for detailed guides.`

// NewMCPServer creates a new MCP server with the provided WhatsApp client and storage.
func NewMCPServer(wa *whatsapp.Client, store *storage.MessageStore, mediaStore *storage.MediaStore, timezone *time.Location) *MCPServer {
	return newMCPServer(wa, store, mediaStore, timezone, false, OwnerInfo{})
}

// NewReadOnlyMCPServer creates an MCP server that serves a read-only data source
// (the native WhatsApp app database) with no live WhatsApp client. Tools that
// require sending or live sync are not registered.
func NewReadOnlyMCPServer(store *storage.MessageStore, mediaStore *storage.MediaStore, owner OwnerInfo, timezone *time.Location) *MCPServer {
	return newMCPServer(nil, store, mediaStore, timezone, true, owner)
}

func newMCPServer(wa *whatsapp.Client, store *storage.MessageStore, mediaStore *storage.MediaStore, timezone *time.Location, readOnly bool, owner OwnerInfo) *MCPServer {
	instructions := fullModeInstructions
	if readOnly {
		instructions = readOnlyModeInstructions
	}

	s := server.NewMCPServer(
		"WhatsApp MCP",
		"1.0.0",
		server.WithInstructions(instructions),
		server.WithToolCapabilities(true),
		server.WithPromptCapabilities(true),
		server.WithResourceCapabilities(true, true),
		server.WithRecovery(),
	)

	m := &MCPServer{
		server:     s,
		wa:         wa,
		store:      store,
		mediaStore: mediaStore,
		log:        log.Default(),
		timezone:   timezone,
		readOnly:   readOnly,
		owner:      owner,
	}

	// register all capabilities
	m.registerTools()
	m.registerPrompts()
	m.registerResources()

	return m
}

// GetServer returns the underlying MCP server instance.
func (m *MCPServer) GetServer() *server.MCPServer {
	return m.server
}
