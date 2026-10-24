package peer

// SelectedPairStats describes the ICE candidate pair currently carrying the
// connection and the application bytes sent over it.
type SelectedPairStats struct {
	// LocalCandidateType and RemoteCandidateType are "host", "srflx",
	// "prflx" or "relay".
	LocalCandidateType  string
	RemoteCandidateType string
	LocalAddress        string
	LocalPort           uint16
	RemoteAddress       string
	RemotePort          uint16
	// Protocol is the candidate transport, "udp" or "tcp".
	Protocol        string
	BytesSent       uint64
	BytesReceived   uint64
	PacketsSent     uint32
	PacketsReceived uint32
}

// Relayed reports whether either side of the pair is a TURN relay candidate.
func (s SelectedPairStats) Relayed() bool {
	const relay = "relay"
	return s.LocalCandidateType == relay || s.RemoteCandidateType == relay
}
