package cmd

import "testing"

func TestNormalizeAudioUploadNameUsesTruthfulParameterizedContentType(t *testing.T) {
	tests := []struct {
		name        string
		filename    string
		contentType string
		want        string
	}{
		{name: "Safari MP4 corrects webm", filename: "recording.webm", contentType: "audio/mp4;codecs=mp4a.40.2", want: "recording.m4a"},
		{name: "M4A remains M4A", filename: "voice.m4a", contentType: "audio/mp4", want: "voice.m4a"},
		{name: "WebM parameters", filename: "voice", contentType: "audio/webm;codecs=opus", want: "voice.webm"},
		{name: "Ogg", filename: "voice.ogg", contentType: "audio/ogg", want: "voice.ogg"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeAudioUploadName(tt.filename, tt.contentType)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("normalizeAudioUploadName() = %q, want %q", got, tt.want)
			}
		})
	}
}
