package relay

// The websocket protocol version a client can ask about with {"type":"hello"}, so it can tell a relay that is
// too old for what it needs (and say so) instead of silently getting less. Clients that never ask (the iOS app
// does not) are unaffected.
//
//	1  the original messages
//	2  start_session / end_session, send_key / send_text / capture_screen, styled screens with scrollback,
//	   list_sessions status snapshots on request
const ProtocolVersion = 2

// ProtocolFeatures names what this relay can do, one string per capability, for clients that care about one
// thing and not the whole version number.
var ProtocolFeatures = []string{
	"start_session",
	"end_session",
	"send_key",
	"send_text",
	"capture_screen",
	"styled_screen",  // capture_screen with styled:true: state, chrome as data, colours
	"screen_history", // capture_screen with history:N: scrollback
	"list_status",    // list_sessions with status:true: the working/idle snapshot
}

func helloMessage() map[string]any {
	return map[string]any{"type": "hello", "protocol": ProtocolVersion, "features": ProtocolFeatures}
}
