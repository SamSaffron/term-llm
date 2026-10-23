// Package peer bridges HTTP requests to a fixed http.Handler over a reliable, ordered WebRTC DataChannel.
//
// The wire protocol is versioned JSON, one frame per DataChannel text
// message, on the channel labelled ChannelLabel:
//
//	{"version":1,"type":"request","id":"1","method":"GET","path":"/x","header":{"Accept":["text/event-stream"]},"body":"<base64>"}
//	{"version":1,"type":"response_start","id":"1","status":200,"header":{...}}
//	{"version":1,"type":"response_body","id":"1","body":"<base64>"}
//	{"version":1,"type":"response_end","id":"1","error":"optional failure"}
//	{"version":1,"type":"cancel","id":"1"}
//
// Request IDs are opaque strings chosen by the requesting peer and unique among
// its in-flight requests. A request carries its whole body in one frame;
// response bodies stream in response_body frames. A response_end with a
// non-empty error means the response failed or was truncated. Either side
// must ignore frames for IDs it does not know about. After it sends cancel,
// the requester must not expect any more frames for that ID.
//
// Server serves one fixed handler. Before any handler code runs, every
// request goes through the integrator's session gate and then its route
// authorization callback. Paths must be safe and relative to the origin.
// Credential, cookie, proxy and hop-by-hop headers sent by the remote peer
// are removed. Client is the matching http.RoundTripper. Endpoint wraps a
// Pion PeerConnection and does non-trickle offer/answer signalling. How the
// SDP is exchanged and authenticated (pairing) is left to the caller.
package peer
