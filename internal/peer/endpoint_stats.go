//go:build !js

package peer

import (
	"fmt"
)

// SelectedPair returns the selected ICE candidate pair and its counters. It
// returns false while no pair has been selected (before ICE connects or
// after close). The candidate types and the counters are read separately and
// may straddle a pair switch during ICE renomination.
func (e *Endpoint) SelectedPair() (SelectedPairStats, bool, error) {
	sctp := e.pc.SCTP()
	if sctp == nil || sctp.Transport() == nil || sctp.Transport().ICETransport() == nil {
		return SelectedPairStats{}, false, nil
	}
	ice := sctp.Transport().ICETransport()
	pair, err := ice.GetSelectedCandidatePair()
	if err != nil {
		return SelectedPairStats{}, false, fmt.Errorf("peer: selected candidate pair: %w", err)
	}
	if pair == nil || pair.Local == nil || pair.Remote == nil {
		return SelectedPairStats{}, false, nil
	}
	counters, ok := ice.GetSelectedCandidatePairStats()
	if !ok {
		return SelectedPairStats{}, false, nil
	}
	return SelectedPairStats{
		LocalCandidateType:  pair.Local.Typ.String(),
		RemoteCandidateType: pair.Remote.Typ.String(),
		LocalAddress:        pair.Local.Address,
		LocalPort:           pair.Local.Port,
		RemoteAddress:       pair.Remote.Address,
		RemotePort:          pair.Remote.Port,
		Protocol:            pair.Local.Protocol.String(),
		BytesSent:           counters.BytesSent,
		BytesReceived:       counters.BytesReceived,
		PacketsSent:         counters.PacketsSent,
		PacketsReceived:     counters.PacketsReceived,
	}, true, nil
}
