package pairing

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func TestV2Vectors(t *testing.T) {
	var v struct {
		HostSeed                     B64    `json:"host_seed"`
		DeviceSeed                   B64    `json:"device_seed"`
		HostKey                      B64    `json:"host_key"`
		DeviceKey                    B64    `json:"device_key"`
		InvitationID                 B64    `json:"invitation_id"`
		PairingSecret                B64    `json:"pairing_secret"`
		DeviceNonce                  B64    `json:"device_nonce"`
		DeviceID                     B64    `json:"device_id"`
		ReconnectCapability          B64    `json:"reconnect_capability"`
		PairRequestTranscript        B64    `json:"pair_request_transcript"`
		MACKey                       B64    `json:"mac_key"`
		PairRequestMAC               B64    `json:"pair_request_mac"`
		PairRequestSignature         B64    `json:"pair_request_signature"`
		PairRequestHash              B64    `json:"pair_request_hash"`
		PairAnswerTranscript         B64    `json:"pair_answer_transcript"`
		PairAnswerMAC                B64    `json:"pair_answer_mac"`
		PairAnswerSignature          B64    `json:"pair_answer_signature"`
		SAS                          string `json:"sas"`
		TransportChallengeID         B64    `json:"transport_challenge_id"`
		TransportHostNonce           B64    `json:"transport_host_nonce"`
		TransportDeviceNonce         B64    `json:"transport_device_nonce"`
		TransportOfferSDP            string `json:"transport_offer_sdp"`
		TransportRequestTranscript   B64    `json:"transport_request_transcript"`
		TransportRequestHash         B64    `json:"transport_request_hash"`
		TransportRequestSignature    B64    `json:"transport_request_signature"`
		TransportHelloNonce          B64    `json:"transport_hello_nonce"`
		TransportHelloTranscript     B64    `json:"transport_hello_transcript"`
		TransportHelloSignature      B64    `json:"transport_hello_signature"`
		TransportChallengeExpiresAt  int64  `json:"transport_challenge_expires_at"`
		TransportChallengeTranscript B64    `json:"transport_challenge_transcript"`
		TransportChallengeSignature  B64    `json:"transport_challenge_signature"`
		TransportAnswerSDP           string `json:"transport_answer_sdp"`
		TransportAnswerTranscript    B64    `json:"transport_answer_transcript"`
		TransportAnswerSignature     B64    `json:"transport_answer_signature"`
		PairRequestJSON              string `json:"pair_request_json"`
		PairAnswerJSON               string `json:"pair_answer_json"`
		TransportHelloJSON           string `json:"transport_hello_json"`
		TransportChallengeJSON       string `json:"transport_challenge_json"`
		TransportRequestJSON         string `json:"transport_request_json"`
		TransportAnswerJSON          string `json:"transport_answer_json"`
	}
	data, err := os.ReadFile("testdata/vectors_v2.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	host, err := IdentityFromSeed(v.HostSeed)
	if err != nil {
		t.Fatal(err)
	}
	device, err := IdentityFromSeed(v.DeviceSeed)
	if err != nil {
		t.Fatal(err)
	}
	assertBytes(t, "host key", host.PublicKey(), v.HostKey)
	assertBytes(t, "device key", device.PublicKey(), v.DeviceKey)
	r := &PairRequest{V: WireVersion, Type: TypePairRequest, InvitationID: v.InvitationID, DeviceKey: v.DeviceKey, DeviceNonce: v.DeviceNonce, DeviceName: "phone"}
	requestTranscript := PairRequestTranscript(r, v.HostKey)
	assertBytes(t, "pair request transcript", requestTranscript, v.PairRequestTranscript)
	assertBytes(t, "mac key", MACKey(v.PairingSecret), v.MACKey)
	assertBytes(t, "pair request MAC", TranscriptMAC(v.PairingSecret, requestTranscript), v.PairRequestMAC)
	assertBytes(t, "pair request signature", device.Sign(requestTranscript), v.PairRequestSignature)
	assertBytes(t, "pair request hash", RequestHash(requestTranscript), v.PairRequestHash)
	r.MAC, r.Sig = v.PairRequestMAC, v.PairRequestSignature
	assertJSON(t, "pair request JSON", r, v.PairRequestJSON)
	a := &PairAnswer{V: WireVersion, Type: TypePairAnswer, InvitationID: v.InvitationID, HostKey: v.HostKey, DeviceKey: v.DeviceKey, DeviceNonce: v.DeviceNonce, RequestHash: v.PairRequestHash, DeviceID: v.DeviceID, ReconnectCapability: v.ReconnectCapability, Scope: ScopeWebV1, BasePath: "/chat", HostLabel: "desk"}
	answerTranscript := PairAnswerTranscript(a)
	assertBytes(t, "pair answer transcript", answerTranscript, v.PairAnswerTranscript)
	assertBytes(t, "pair answer MAC", TranscriptMAC(v.PairingSecret, answerTranscript), v.PairAnswerMAC)
	assertBytes(t, "pair answer signature", host.Sign(answerTranscript), v.PairAnswerSignature)
	a.MAC, a.Sig = v.PairAnswerMAC, v.PairAnswerSignature
	assertJSON(t, "pair answer JSON", a, v.PairAnswerJSON)
	sas, err := ShortAuthenticationString(v.PairingSecret, v.InvitationID, v.HostKey, v.DeviceKey)
	if err != nil {
		t.Fatal(err)
	}
	if sas != v.SAS {
		t.Fatalf("SAS = %q, want %q", sas, v.SAS)
	}
	hello := &TransportHello{V: WireVersion, Type: TypeTransportHello, DeviceKey: v.DeviceKey, DeviceID: v.DeviceID, ReconnectCapability: v.ReconnectCapability, DeviceNonce: v.TransportHelloNonce}
	helloTranscript := TransportHelloTranscript(hello, v.HostKey)
	assertBytes(t, "transport hello transcript", helloTranscript, v.TransportHelloTranscript)
	assertBytes(t, "transport hello signature", device.Sign(helloTranscript), v.TransportHelloSignature)
	hello.Sig = v.TransportHelloSignature
	assertJSON(t, "transport hello JSON", hello, v.TransportHelloJSON)
	challenge := &TransportChallenge{V: WireVersion, Type: TypeTransportChallenge, ChallengeID: v.TransportChallengeID, HostKey: v.HostKey, HostNonce: v.TransportHostNonce, DeviceKey: v.DeviceKey, DeviceID: v.DeviceID, ExpiresAt: v.TransportChallengeExpiresAt}
	challengeTranscript := TransportChallengeTranscript(challenge)
	assertBytes(t, "transport challenge transcript", challengeTranscript, v.TransportChallengeTranscript)
	assertBytes(t, "transport challenge signature", host.Sign(challengeTranscript), v.TransportChallengeSignature)
	challenge.Sig = v.TransportChallengeSignature
	assertJSON(t, "transport challenge JSON", challenge, v.TransportChallengeJSON)
	tr := &TransportRequest{V: WireVersion, Type: TypeTransportRequest, ChallengeID: v.TransportChallengeID, HostNonce: v.TransportHostNonce, DeviceKey: v.DeviceKey, DeviceNonce: v.TransportDeviceNonce, DeviceID: v.DeviceID, OfferSDP: v.TransportOfferSDP}
	transportTranscript := TransportRequestTranscript(tr, v.HostKey)
	assertBytes(t, "transport request transcript", transportTranscript, v.TransportRequestTranscript)
	assertBytes(t, "transport request hash", RequestHash(transportTranscript), v.TransportRequestHash)
	assertBytes(t, "transport request signature", device.Sign(transportTranscript), v.TransportRequestSignature)
	tr.Sig = v.TransportRequestSignature
	assertJSON(t, "transport request JSON", tr, v.TransportRequestJSON)
	ta := &TransportAnswer{V: WireVersion, Type: TypeTransportAnswer, ChallengeID: v.TransportChallengeID, HostKey: v.HostKey, HostNonce: v.TransportHostNonce, DeviceKey: v.DeviceKey, DeviceNonce: v.TransportDeviceNonce, RequestHash: v.TransportRequestHash, DeviceID: v.DeviceID, AnswerSDP: v.TransportAnswerSDP}
	answerTransportTranscript := TransportAnswerTranscript(ta)
	assertBytes(t, "transport answer transcript", answerTransportTranscript, v.TransportAnswerTranscript)
	assertBytes(t, "transport answer signature", host.Sign(answerTransportTranscript), v.TransportAnswerSignature)
	ta.Sig = v.TransportAnswerSignature
	assertJSON(t, "transport answer JSON", ta, v.TransportAnswerJSON)
}

func assertJSON(t *testing.T, name string, value any, want string) {
	t.Helper()
	got, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s mismatch\n got %s\nwant %s", name, got, want)
	}
}

func assertBytes(t *testing.T, name string, got, want []byte) {
	t.Helper()
	if !bytes.Equal(got, want) {
		t.Fatalf("%s mismatch\n got %x\nwant %x", name, got, want)
	}
}
