package pairing

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// vectorFile is a cross-language fixture: native clients should reproduce
// every hex value from the listed inputs.
type vectorFile struct {
	HostSeed           string `json:"host_seed_hex"`
	DeviceSeed         string `json:"device_seed_hex"`
	HostKey            string `json:"host_key_hex"`
	DeviceKey          string `json:"device_key_hex"`
	Secret             string `json:"secret_hex"`
	SessionID          string `json:"sid_hex"`
	HostNonce          string `json:"hn_hex"`
	DeviceNonce        string `json:"device_nonce_hex"`
	DeviceName         string `json:"device_name"`
	OfferSDP           string `json:"offer_sdp"`
	AnswerSDP          string `json:"answer_sdp"`
	GrantID            string `json:"grant_id_hex"`
	GrantExpiresAt     int64  `json:"grant_expires_at"`
	MACKey             string `json:"mac_key_hex"`
	RequestTranscript  string `json:"pair_request_transcript_hex"`
	RequestMAC         string `json:"pair_request_mac_hex"`
	RequestSig         string `json:"pair_request_sig_hex"`
	RequestHash        string `json:"request_hash_hex"`
	AnswerTranscript   string `json:"pair_answer_transcript_hex"`
	AnswerMAC          string `json:"pair_answer_mac_hex"`
	AnswerSig          string `json:"pair_answer_sig_hex"`
	RequestJSON        string `json:"pair_request_json"`
	ReconnectChallenge string `json:"reconnect_challenge_id_hex"`
	ReconnectTranscipt string `json:"reconnect_request_transcript_hex"`
	ReconnectSig       string `json:"reconnect_request_sig_hex"`
}

func fill(b byte, n int) []byte { return bytes.Repeat([]byte{b}, n) }

func computeVectors(t *testing.T) vectorFile {
	host, _ := IdentityFromSeed(fill(0x01, 32))
	dev, _ := IdentityFromSeed(fill(0x02, 32))
	secret := fill(0x03, SecretSize)
	req := PairRequest{V: Version, Type: TypePairRequest, SessionID: fill(0x04, 16), HostNonce: fill(0x05, 32),
		DeviceKey: B64(dev.PublicKey()), DeviceNonce: fill(0x06, 32), DeviceName: "Sam's phone", OfferSDP: sdp(fpA)}
	rt := pairRequestTranscript(&req, host.PublicKey())
	req.MAC = TranscriptMAC(secret, rt)
	req.Sig = dev.sign(rt)
	rh := hashTranscript(rt)
	ans := PairAnswer{V: Version, Type: TypePairAnswer, SessionID: req.SessionID, HostKey: B64(host.PublicKey()),
		HostNonce: req.HostNonce, DeviceKey: req.DeviceKey, DeviceNonce: req.DeviceNonce, RequestHash: rh,
		GrantID: fill(0x07, 16), GrantExpiresAt: 1_900_000_000, AnswerSDP: sdp(fpB)}
	at := pairAnswerTranscript(&ans)
	reqJSON, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	rr := ReconnectRequest{V: Version, Type: TypeReconnectRequest, ChallengeID: fill(0x08, 16), HostNonce: fill(0x09, 32),
		DeviceKey: B64(dev.PublicKey()), DeviceNonce: fill(0x0a, 32), GrantID: ans.GrantID, OfferSDP: sdp(fpA)}
	rrt := reconnectRequestTranscript(&rr, host.PublicKey())
	return vectorFile{
		HostSeed: hex.EncodeToString(fill(0x01, 32)), DeviceSeed: hex.EncodeToString(fill(0x02, 32)),
		HostKey: hex.EncodeToString(host.PublicKey()), DeviceKey: hex.EncodeToString(dev.PublicKey()),
		Secret: hex.EncodeToString(secret), SessionID: hex.EncodeToString(req.SessionID),
		HostNonce: hex.EncodeToString(req.HostNonce), DeviceNonce: hex.EncodeToString(req.DeviceNonce),
		DeviceName: req.DeviceName, OfferSDP: req.OfferSDP, AnswerSDP: ans.AnswerSDP,
		GrantID: hex.EncodeToString(ans.GrantID), GrantExpiresAt: ans.GrantExpiresAt,
		MACKey:            hex.EncodeToString(macKeyFor(secret)),
		RequestTranscript: hex.EncodeToString(rt), RequestMAC: hex.EncodeToString(req.MAC),
		RequestSig: hex.EncodeToString(req.Sig), RequestHash: hex.EncodeToString(rh),
		AnswerTranscript: hex.EncodeToString(at), AnswerMAC: hex.EncodeToString(TranscriptMAC(secret, at)),
		AnswerSig: hex.EncodeToString(host.sign(at)), RequestJSON: string(reqJSON),
		ReconnectChallenge: hex.EncodeToString(rr.ChallengeID), ReconnectTranscipt: hex.EncodeToString(rrt),
		ReconnectSig: hex.EncodeToString(dev.sign(rrt)),
	}
}

// TestWireVectors pins the v1 encoding. Regenerate only for a new wire version:
// PAIRING_UPDATE_VECTORS=1 make test PKG=./internal/pairing RUN=TestWireVectors
func TestWireVectors(t *testing.T) {
	got := computeVectors(t)
	path := filepath.Join("testdata", "vectors_v1.json")
	if os.Getenv("PAIRING_UPDATE_VECTORS") == "1" {
		data, _ := json.MarshalIndent(got, "", "  ")
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want vectorFile
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("wire vectors changed; v1 is frozen\n got: %+v\nwant: %+v", got, want)
	}
}
