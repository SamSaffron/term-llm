package ansisafe

import "testing"

func TestSuffixString(t *testing.T) {
	tests := []struct {
		name string
		s    string
		pos  int
		want string
	}{
		{
			name: "safe split",
			s:    "hello\x1b[31mred\x1b[0mworld",
			pos:  5,
			want: "\x1b[31mred\x1b[0mworld",
		},
		{
			name: "pos zero",
			s:    "hello",
			pos:  0,
			want: "hello",
		},
		{
			name: "pos negative",
			s:    "hello",
			pos:  -1,
			want: "hello",
		},
		{
			name: "mid ansi params",
			s:    "hello\x1b[38;5;178mcolored\x1b[0m",
			pos:  13, // inside 38;5;178
			want: "colored\x1b[0m",
		},
		{
			name: "beyond length",
			s:    "hello",
			pos:  99,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SuffixString(tt.s, tt.pos)
			if got != tt.want {
				t.Fatalf("SuffixString(%q, %d) = %q, want %q", tt.s, tt.pos, got, tt.want)
			}
		})
	}
}

func TestSuffixBytes(t *testing.T) {
	input := []byte("hello\x1b[38;5;178mcolored\x1b[0m")
	got := SuffixBytes(input, 13) // inside ANSI sequence
	want := []byte("colored\x1b[0m")
	if string(got) != string(want) {
		t.Fatalf("SuffixBytes mismatch: got %q want %q", string(got), string(want))
	}
}
