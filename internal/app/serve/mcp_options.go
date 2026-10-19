package serve

// MCPOptions holds the plain-data configuration for the serve mcp surface.
type MCPOptions struct {
	Host       string
	Port       int
	Token      string
	Tools      string
	EditFormat string
	ReadDirs   []string
	WriteDirs  []string
	ShellAllow []string
	Approval   string
	Yolo       bool
	Auto       bool
	Debug      bool
}
