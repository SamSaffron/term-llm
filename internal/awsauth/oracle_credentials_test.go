package awsauth

// Fixtures in testdata/ were generated with aws-sdk-go-v2 as an oracle (core
// v1.47.1, config v1.33.6, eventstream v1.7.20) before the dependency was removed.

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

func TestCredentialsRegionOracle(t *testing.T) {
	var cases []struct {
		Name, Profile, Region, AccessKeyID, SecretAccessKey, SessionToken string
		Env                                                               map[string]string
		Config, SharedCredentials                                         string
		ExpectedRegion, ExpectedAccessKeyID                               string
	}
	b, err := os.ReadFile("testdata/credentials.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			o := testOptions(t, c.Env, c.Config, c.SharedCredentials)
			o.Profile, o.Region = c.Profile, c.Region
			o.AccessKeyID, o.SecretAccessKey, o.SessionToken = c.AccessKeyID, c.SecretAccessKey, c.SessionToken
			resolved, err := Resolve(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			value, err := resolved.Credentials.Retrieve(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if resolved.Region != c.ExpectedRegion || value.AccessKeyID != c.ExpectedAccessKeyID {
				t.Errorf("region/key = %q/%q want %q/%q", resolved.Region, value.AccessKeyID, c.ExpectedRegion, c.ExpectedAccessKeyID)
			}
		})
	}
}
