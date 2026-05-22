package mcp

// BundledServer represents a curated MCP server included in the binary.
// These are popular servers that may not always appear in registry searches.
type BundledServer struct {
	Name        string
	Description string
	Package     string // npm package name
	PackageType string // "npm" or "pypi"
	Category    string
	Official    bool   // true if official/reference implementation
	URL         string // repository or homepage URL
	RemoteURL   string // streamable HTTP MCP endpoint, when available
}

// bundledServers contains curated MCP servers organized by category.
// These are merged with registry results to ensure important servers are always visible.
// Sourced from: https://github.com/modelcontextprotocol/servers,
// https://github.com/punkpeye/awesome-mcp-servers, and https://mcpservers.org
var bundledServers = []BundledServer{
	// === Official/Reference Servers ===
	{
		Name:        "filesystem",
		Description: "Secure file operations with configurable access controls",
		Package:     "@modelcontextprotocol/server-filesystem",
		PackageType: "npm",
		Category:    "Reference",
		Official:    true,
		URL:         "https://github.com/modelcontextprotocol/servers",
	},
	{
		Name:        "git",
		Description: "Tools to read, search, and manipulate Git repositories",
		Package:     "@modelcontextprotocol/server-git",
		PackageType: "npm",
		Category:    "Reference",
		Official:    true,
		URL:         "https://github.com/modelcontextprotocol/servers",
	},
	{
		Name:        "memory",
		Description: "Knowledge graph-based persistent memory system",
		Package:     "@modelcontextprotocol/server-memory",
		PackageType: "npm",
		Category:    "Reference",
		Official:    true,
		URL:         "https://github.com/modelcontextprotocol/servers",
	},
	{
		Name:        "fetch",
		Description: "Web content fetching and conversion for efficient LLM usage",
		Package:     "@modelcontextprotocol/server-fetch",
		PackageType: "npm",
		Category:    "Reference",
		Official:    true,
		URL:         "https://github.com/modelcontextprotocol/servers",
	},
	{
		Name:        "puppeteer",
		Description: "Browser automation with Puppeteer for web scraping and interaction",
		Package:     "@modelcontextprotocol/server-puppeteer",
		PackageType: "npm",
		Category:    "Reference",
		Official:    true,
		URL:         "https://github.com/modelcontextprotocol/servers",
	},
	{
		Name:        "sequential-thinking",
		Description: "Dynamic and reflective problem-solving through thought sequences",
		Package:     "@modelcontextprotocol/server-sequential-thinking",
		PackageType: "npm",
		Category:    "Reference",
		Official:    true,
		URL:         "https://github.com/modelcontextprotocol/servers",
	},
	{
		Name:        "time",
		Description: "Time and timezone conversion capabilities",
		Package:     "@modelcontextprotocol/server-time",
		PackageType: "npm",
		Category:    "Reference",
		Official:    true,
		URL:         "https://github.com/modelcontextprotocol/servers",
	},
	{
		Name:        "everything",
		Description: "Reference/test server with prompts, resources, and tools",
		Package:     "@modelcontextprotocol/server-everything",
		PackageType: "npm",
		Category:    "Reference",
		Official:    true,
		URL:         "https://github.com/modelcontextprotocol/servers",
	},
	// === Search ===
	{
		Name:        "exa",
		Description: "Exa web search and page fetching via the free remote MCP server",
		Package:     "exa-mcp-server",
		PackageType: "npm",
		Category:    "Search",
		Official:    true,
		URL:         "https://github.com/exa-labs/exa-mcp-server",
		RemoteURL:   "https://mcp.exa.ai/mcp",
	},

	// === Browser Automation ===
	{
		Name:        "playwright",
		Description: "Browser automation via Playwright with accessibility snapshots",
		Package:     "@playwright/mcp",
		PackageType: "npm",
		Category:    "Browser",
		Official:    false,
		URL:         "https://github.com/microsoft/playwright-mcp",
	},
	{
		Name:        "browserbase",
		Description: "Cloud browser automation for navigation, forms, and screenshots",
		Package:     "@browserbasehq/mcp-server-browserbase",
		PackageType: "npm",
		Category:    "Browser",
		Official:    false,
		URL:         "https://github.com/browserbase/mcp-server-browserbase",
	},
	{
		Name:        "browser-use",
		Description: "Lightweight browser automation with browser-use library",
		Package:     "browser-use-mcp-server",
		PackageType: "pypi",
		Category:    "Browser",
		Official:    false,
		URL:         "https://github.com/co-browser/browser-use-mcp-server",
	},

	// === Databases ===
	{
		Name:        "postgres",
		Description: "PostgreSQL database operations with read-only safety mode",
		Package:     "@modelcontextprotocol/server-postgres",
		PackageType: "npm",
		Category:    "Database",
		Official:    true,
		URL:         "https://github.com/modelcontextprotocol/servers",
	},
	{
		Name:        "sqlite",
		Description: "SQLite database operations and business intelligence",
		Package:     "@modelcontextprotocol/server-sqlite",
		PackageType: "npm",
		Category:    "Database",
		Official:    true,
		URL:         "https://github.com/modelcontextprotocol/servers",
	},
	{
		Name:        "supabase",
		Description: "Supabase database, auth, and edge functions integration",
		Package:     "@supabase/mcp-server-supabase",
		PackageType: "npm",
		Category:    "Database",
		Official:    false,
		URL:         "https://github.com/supabase/mcp-server-supabase",
	},
	{
		Name:        "neon",
		Description: "Serverless Postgres platform interaction",
		Package:     "@neondatabase/mcp-server-neon",
		PackageType: "npm",
		Category:    "Database",
		Official:    false,
		URL:         "https://github.com/neondatabase/mcp-server-neon",
	},
	{
		Name:        "redis",
		Description: "Redis database operations and caching",
		Package:     "@redis/mcp-server",
		PackageType: "npm",
		Category:    "Database",
		Official:    false,
		URL:         "https://github.com/redis/mcp-redis-cloud",
	},
	{
		Name:        "mongodb",
		Description: "MongoDB database operations and queries",
		Package:     "mongodb-mcp-server",
		PackageType: "npm",
		Category:    "Database",
		Official:    false,
		URL:         "https://github.com/mongodb/mongodb-mcp-server",
	},
	{
		Name:        "neo4j",
		Description: "Graph database with schema inspection and Cypher queries",
		Package:     "@neo4j/mcp-neo4j",
		PackageType: "npm",
		Category:    "Database",
		Official:    false,
		URL:         "https://github.com/neo4j/mcp-neo4j",
	},
	{
		Name:        "motherduck",
		Description: "Query and analyze data with MotherDuck and local DuckDB",
		Package:     "@motherduck/mcp-server-motherduck",
		PackageType: "npm",
		Category:    "Database",
		Official:    false,
		URL:         "https://github.com/motherduckdb/mcp-server-motherduck",
	},

	// === Vector Databases & Search ===
	{
		Name:        "qdrant",
		Description: "Semantic memory layer with Qdrant vector search engine",
		Package:     "@qdrant/mcp-server-qdrant",
		PackageType: "npm",
		Category:    "Vector DB",
		Official:    false,
		URL:         "https://github.com/qdrant/mcp-server-qdrant",
	},
	{
		Name:        "chroma",
		Description: "Embeddings, vector search, and document storage",
		Package:     "chroma-mcp",
		PackageType: "pypi",
		Category:    "Vector DB",
		Official:    false,
		URL:         "https://github.com/chroma-core/chroma-mcp",
	},
	{
		Name:        "milvus",
		Description: "Search and interact with data in Milvus Vector Database",
		Package:     "mcp-server-milvus",
		PackageType: "pypi",
		Category:    "Vector DB",
		Official:    false,
		URL:         "https://github.com/milvus-io/mcp-server-milvus",
	},

	// === Cloud Platforms ===
	{
		Name:        "aws",
		Description: "AWS services integration including S3, Lambda, and more",
		Package:     "@aws/mcp",
		PackageType: "npm",
		Category:    "Cloud",
		Official:    false,
		URL:         "https://github.com/awslabs/mcp",
	},
	{
		Name:        "cloudflare",
		Description: "Deploy and configure Cloudflare Workers, KV, R2, D1",
		Package:     "@cloudflare/mcp-server-cloudflare",
		PackageType: "npm",
		Category:    "Cloud",
		Official:    false,
		URL:         "https://github.com/cloudflare/mcp-server-cloudflare",
	},
	{
		Name:        "azure",
		Description: "Azure CLI wrapper for cloud resource management",
		Package:     "mcp-server-azure-cli",
		PackageType: "npm",
		Category:    "Cloud",
		Official:    false,
		URL:         "https://github.com/jdubois/azure-cli-mcp",
	},
	{
		Name:        "google-cloud-run",
		Description: "Deploy applications to Google Cloud Run",
		Package:     "@anthropic/mcp-server-cloud-run",
		PackageType: "npm",
		Category:    "Cloud",
		Official:    false,
		URL:         "https://github.com/anthropics/anthropic-quickstarts",
	},

	// === Kubernetes & Containers ===
	{
		Name:        "kubernetes",
		Description: "Kubernetes cluster operations for pods, deployments, and services",
		Package:     "@flux159/mcp-server-kubernetes",
		PackageType: "npm",
		Category:    "Containers",
		Official:    false,
		URL:         "https://github.com/Flux159/mcp-server-kubernetes",
	},
	{
		Name:        "docker",
		Description: "Docker container and image management",
		Package:     "mcp-server-docker",
		PackageType: "npm",
		Category:    "Containers",
		Official:    false,
		URL:         "https://github.com/docker/mcp-server-docker",
	},

	// === Developer Tools ===
	{
		Name:        "github",
		Description: "GitHub repository, issue, and PR management",
		Package:     "@modelcontextprotocol/server-github",
		PackageType: "npm",
		Category:    "DevTools",
		Official:    true,
		URL:         "https://github.com/modelcontextprotocol/servers",
	},
	{
		Name:        "gitlab",
		Description: "GitLab repository and CI/CD integration",
		Package:     "@modelcontextprotocol/server-gitlab",
		PackageType: "npm",
		Category:    "DevTools",
		Official:    true,
		URL:         "https://github.com/modelcontextprotocol/servers",
	},
	{
		Name:        "sentry",
		Description: "Sentry error tracking and performance monitoring",
		Package:     "@sentry/mcp-server-sentry",
		PackageType: "npm",
		Category:    "DevTools",
		Official:    false,
		URL:         "https://github.com/getsentry/sentry-mcp",
	},
	{
		Name:        "semgrep",
		Description: "Code analysis and security scanning with Semgrep",
		Package:     "@semgrep/mcp-server-semgrep",
		PackageType: "npm",
		Category:    "DevTools",
		Official:    false,
		URL:         "https://github.com/semgrep/semgrep-mcp",
	},
	{
		Name:        "linear",
		Description: "Linear issue tracking and project management",
		Package:     "@linear/mcp-server-linear",
		PackageType: "npm",
		Category:    "DevTools",
		Official:    false,
		URL:         "https://github.com/linear/linear-mcp",
	},
	{
		Name:        "circleci",
		Description: "CircleCI pipeline and build management",
		Package:     "@circleci/mcp-server-circleci",
		PackageType: "npm",
		Category:    "DevTools",
		Official:    false,
		URL:         "https://github.com/circleci/mcp-server-circleci",
	},
	{
		Name:        "buildkite",
		Description: "Buildkite pipeline and build management",
		Package:     "@buildkite/mcp-server-buildkite",
		PackageType: "npm",
		Category:    "DevTools",
		Official:    false,
		URL:         "https://github.com/buildkite/mcp-server-buildkite",
	},

	// === Productivity & Communication ===
	{
		Name:        "notion",
		Description: "Notion workspace pages and databases integration",
		Package:     "@notionhq/mcp-server-notion",
		PackageType: "npm",
		Category:    "Productivity",
		Official:    false,
		URL:         "https://github.com/notionhq/mcp-server-notion",
	},
	{
		Name:        "slack",
		Description: "Slack workspace messaging and channel management",
		Package:     "@modelcontextprotocol/server-slack",
		PackageType: "npm",
		Category:    "Productivity",
		Official:    true,
		URL:         "https://github.com/modelcontextprotocol/servers",
	},
	{
		Name:        "google-drive",
		Description: "Google Drive file access and management",
		Package:     "@modelcontextprotocol/server-google-drive",
		PackageType: "npm",
		Category:    "Productivity",
		Official:    true,
		URL:         "https://github.com/modelcontextprotocol/servers",
	},
	{
		Name:        "gmail",
		Description: "Gmail email reading and sending",
		Package:     "mcp-gmail",
		PackageType: "npm",
		Category:    "Productivity",
		Official:    false,
		URL:         "https://github.com/nicholasrodriguez/mcp-gmail",
	},
	{
		Name:        "obsidian",
		Description: "Obsidian vault notes access and manipulation",
		Package:     "mcp-obsidian",
		PackageType: "npm",
		Category:    "Productivity",
		Official:    false,
		URL:         "https://github.com/smithery-ai/mcp-obsidian",
	},
	{
		Name:        "todoist",
		Description: "Todoist task and project management",
		Package:     "mcp-todoist",
		PackageType: "npm",
		Category:    "Productivity",
		Official:    false,
		URL:         "https://github.com/abhiz123/todoist-mcp-server",
	},
	{
		Name:        "discourse",
		Description: "Search and interact with Discourse forum posts and topics",
		Package:     "@discourse/mcp",
		PackageType: "npm",
		Category:    "Productivity",
		Official:    true,
		URL:         "https://github.com/discourse/mcp",
	},

	// === Search & Web ===
	{
		Name:        "brave-search",
		Description: "Brave Search API for web and local search",
		Package:     "@modelcontextprotocol/server-brave-search",
		PackageType: "npm",
		Category:    "Search",
		Official:    true,
		URL:         "https://github.com/modelcontextprotocol/servers",
	},
	{
		Name:        "exa",
		Description: "Exa AI-powered search engine",
		Package:     "@anthropic/mcp-server-exa",
		PackageType: "npm",
		Category:    "Search",
		Official:    false,
		URL:         "https://github.com/exa-labs/exa-mcp-server",
	},
	{
		Name:        "tavily",
		Description: "Tavily AI search engine for research and content extraction",
		Package:     "@tavily/mcp-server-tavily",
		PackageType: "npm",
		Category:    "Search",
		Official:    false,
		URL:         "https://github.com/tavily-ai/tavily-mcp",
	},
	{
		Name:        "perplexity",
		Description: "Perplexity Sonar API for real-time research",
		Package:     "mcp-server-perplexity",
		PackageType: "npm",
		Category:    "Search",
		Official:    false,
		URL:         "https://github.com/ppl-ai/mcp-server-perplexity",
	},
	{
		Name:        "firecrawl",
		Description: "Web scraping and crawling with Firecrawl",
		Package:     "@anthropic/mcp-server-firecrawl",
		PackageType: "npm",
		Category:    "Search",
		Official:    false,
		URL:         "https://github.com/firecrawl/firecrawl-mcp-server",
	},

	// === Finance & Crypto ===
	{
		Name:        "stripe",
		Description: "Stripe payment processing and subscription management",
		Package:     "@stripe/mcp-server-stripe",
		PackageType: "npm",
		Category:    "Finance",
		Official:    false,
		URL:         "https://github.com/stripe/mcp-server-stripe",
	},
	{
		Name:        "coinbase",
		Description: "Coinbase cryptocurrency trading and wallet management",
		Package:     "@coinbase/mcp-server-coinbase",
		PackageType: "npm",
		Category:    "Finance",
		Official:    false,
		URL:         "https://github.com/coinbase/mcp-server-coinbase",
	},
	{
		Name:        "coingecko",
		Description: "Cryptocurrency price and market data",
		Package:     "mcp-server-coingecko",
		PackageType: "npm",
		Category:    "Finance",
		Official:    false,
		URL:         "https://github.com/coingecko/mcp-server-coingecko",
	},
	{
		Name:        "alpha-vantage",
		Description: "Financial market data: stocks, ETFs, forex, crypto",
		Package:     "mcp-server-alpha-vantage",
		PackageType: "npm",
		Category:    "Finance",
		Official:    false,
		URL:         "https://github.com/alphavantage/mcp-server",
	},

	// === Code Execution & Sandboxing ===
	{
		Name:        "e2b",
		Description: "Run code in secure cloud sandboxes",
		Package:     "@e2b/mcp-server-e2b",
		PackageType: "npm",
		Category:    "Sandbox",
		Official:    false,
		URL:         "https://github.com/e2b-dev/mcp-server-e2b",
	},
	{
		Name:        "riza",
		Description: "Arbitrary code execution platform with security",
		Package:     "@riza/mcp-server-riza",
		PackageType: "npm",
		Category:    "Sandbox",
		Official:    false,
		URL:         "https://github.com/riza-io/mcp-server-riza",
	},

	// === AI & LLM Tools ===
	{
		Name:        "context7",
		Description: "Up-to-date documentation injection for AI coding assistants",
		Package:     "@upstash/context7-mcp",
		PackageType: "npm",
		Category:    "AI Tools",
		Official:    false,
		URL:         "https://github.com/upstash/context7",
	},
	{
		Name:        "langfuse",
		Description: "LLM observability, prompt management, and analytics",
		Package:     "@langfuse/mcp-server-langfuse",
		PackageType: "npm",
		Category:    "AI Tools",
		Official:    false,
		URL:         "https://github.com/langfuse/mcp-server-langfuse",
	},
	{
		Name:        "elevenlabs",
		Description: "ElevenLabs text-to-speech and voice generation",
		Package:     "@elevenlabs/mcp-server-elevenlabs",
		PackageType: "npm",
		Category:    "AI Tools",
		Official:    false,
		URL:         "https://github.com/elevenlabs/mcp-server-elevenlabs",
	},

	// === Data & Analytics ===
	{
		Name:        "snowflake",
		Description: "Snowflake data warehouse queries and operations",
		Package:     "mcp-server-snowflake",
		PackageType: "pypi",
		Category:    "Analytics",
		Official:    false,
		URL:         "https://github.com/snowflake-labs/mcp-server-snowflake",
	},
	{
		Name:        "dbt",
		Description: "dbt (data build tool) project management",
		Package:     "@dbt/mcp-server-dbt",
		PackageType: "npm",
		Category:    "Analytics",
		Official:    false,
		URL:         "https://github.com/dbt-labs/mcp-server-dbt",
	},
	{
		Name:        "bigquery",
		Description: "Google BigQuery data warehouse operations",
		Package:     "mcp-server-bigquery",
		PackageType: "pypi",
		Category:    "Analytics",
		Official:    false,
		URL:         "https://github.com/ergut/mcp-bigquery-server",
	},

	// === Media & Content ===
	{
		Name:        "youtube-transcript",
		Description: "YouTube video subtitles and transcripts",
		Package:     "mcp-youtube-transcript",
		PackageType: "npm",
		Category:    "Media",
		Official:    false,
		URL:         "https://github.com/kimtaeyoon83/mcp-server-youtube-transcript",
	},
	{
		Name:        "spotify",
		Description: "Spotify music library and playback control",
		Package:     "mcp-server-spotify",
		PackageType: "npm",
		Category:    "Media",
		Official:    false,
		URL:         "https://github.com/varunneal/spotify-mcp",
	},
	{
		Name:        "blender",
		Description: "Blender 3D software automation and scripting",
		Package:     "blender-mcp",
		PackageType: "pypi",
		Category:    "Media",
		Official:    false,
		URL:         "https://github.com/ahujasid/blender-mcp",
	},

	// === Infrastructure & Monitoring ===
	{
		Name:        "datadog",
		Description: "Datadog monitoring, metrics, and alerting",
		Package:     "@datadog/mcp-server-datadog",
		PackageType: "npm",
		Category:    "Monitoring",
		Official:    false,
		URL:         "https://github.com/DataDog/mcp-server-datadog",
	},
	{
		Name:        "pagerduty",
		Description: "PagerDuty incident management and alerting",
		Package:     "@pagerduty/mcp-server-pagerduty",
		PackageType: "npm",
		Category:    "Monitoring",
		Official:    false,
		URL:         "https://github.com/PagerDuty/mcp-server-pagerduty",
	},
	{
		Name:        "grafana",
		Description: "Grafana dashboards and alerting",
		Package:     "mcp-grafana",
		PackageType: "npm",
		Category:    "Monitoring",
		Official:    false,
		URL:         "https://github.com/grafana/mcp-grafana",
	},

	// === Aggregators & Meta-MCP ===
	{
		Name:        "pipedream",
		Description: "Connect 2500+ APIs through 8000+ prebuilt tools",
		Package:     "@pipedream/mcp",
		PackageType: "npm",
		Category:    "Aggregator",
		Official:    false,
		URL:         "https://github.com/PipedreamHQ/pipedream",
	},
	{
		Name:        "mindsdb",
		Description: "Connect and unify data across platforms with AI",
		Package:     "mindsdb-mcp",
		PackageType: "pypi",
		Category:    "Aggregator",
		Official:    false,
		URL:         "https://github.com/mindsdb/mindsdb",
	},
}

// GetBundledServers returns all bundled servers.
func GetBundledServers() []BundledServer {
	return bundledServers
}

// GetBundledServersByCategory returns servers grouped by category.
func GetBundledServersByCategory() map[string][]BundledServer {
	result := make(map[string][]BundledServer)
	for _, s := range bundledServers {
		result[s.Category] = append(result[s.Category], s)
	}
	return result
}

// ToRegistryServer converts a BundledServer to a RegistryServer for UI consistency.
func (b *BundledServer) ToRegistryServer() RegistryServer {
	pkgType := b.PackageType
	if pkgType == "" {
		pkgType = "npm"
	}

	return RegistryServer{
		Name:        b.Name,
		Description: b.Description,
		Packages: []PackageInfo{
			{
				RegistryType: pkgType,
				Identifier:   b.Package,
			},
		},
		Repository: &RepositoryInfo{
			URL: b.URL,
		},
	}
}

// ToServerConfig converts a BundledServer to a local ServerConfig.
func (b *BundledServer) ToServerConfig() ServerConfig {
	if b.RemoteURL != "" {
		return ServerConfig{
			Type: "http",
			URL:  b.RemoteURL,
		}
	}

	cfg := ServerConfig{
		Env: make(map[string]string),
	}

	if b.PackageType == "pypi" {
		cfg.Command = "uvx"
		cfg.Args = []string{b.Package}
	} else {
		cfg.Command = "npx"
		cfg.Args = []string{"-y", b.Package}
	}

	return cfg
}

// GetBundledAsRegistryWrappers returns bundled servers as RegistryServerWrappers for UI.
func GetBundledAsRegistryWrappers() []RegistryServerWrapper {
	servers := GetBundledServers()
	result := make([]RegistryServerWrapper, len(servers))
	for i, s := range servers {
		result[i] = RegistryServerWrapper{
			Server: s.ToRegistryServer(),
		}
	}
	return result
}
