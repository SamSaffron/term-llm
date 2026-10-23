package cmd

import (
	"testing"
)

func TestPeerPilotCommandsExistAndRequireCapabilities(t *testing.T) {
	for _, path := range [][]string{{"contain", "pair"}, {"serve", "peer-broker"}} {
		c, _, err := rootCmd.Find(path)
		if err != nil || c == nil {
			t.Fatalf("find %v: %v", path, err)
		}
		flag := c.Flags().Lookup("registration-token-file")
		if flag == nil || len(flag.Annotations["cobra_annotation_bash_completion_one_required_flag"]) == 0 {
			t.Fatalf("%v missing required private registration cap", path)
		}
	}
}
func TestPeerBrokerRejectsUnsafeConfiguration(t *testing.T) {
	valid := peerBrokerOptions{host: "127.0.0.1", port: 8091, maxSessions: 16}
	if err := validatePeerBrokerOptions(valid); err != nil {
		t.Fatal(err)
	}
	tests := []peerBrokerOptions{valid, valid, valid, valid, valid, valid}
	tests[0].host = "0.0.0.0"
	tests[1].host = "example.com"
	tests[2].port = 0
	tests[3].maxSessions = 1000
	tests[4].tlsCert = "certificate-without-key"
	tests[5].cloudflareKeyID = "key-without-token"
	for _, o := range tests {
		if err := validatePeerBrokerOptions(o); err == nil {
			t.Fatalf("accepted unsafe options %+v", o)
		}
	}
	public := valid
	public.host = "0.0.0.0"
	public.tlsCert = "cert"
	public.tlsKey = "key"
	if err := validatePeerBrokerOptions(public); err != nil {
		t.Fatal(err)
	}
}
