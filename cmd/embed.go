package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/embedding"
	"github.com/samsaffron/term-llm/internal/input"
	"github.com/spf13/cobra"
)

var (
	embedProvider   string
	embedFiles      []string
	embedOutput     string
	embedFormat     string
	embedDimensions int
	embedTaskType   string
	embedSimilarity bool
)

var embedCmd = &cobra.Command{
	Use:   "embed [text ...]",
	Short: "Generate text embeddings using AI",
	Long: `Generate text embeddings (vector representations) from text input.

Embeddings capture semantic meaning of text as arrays of floating-point
numbers, useful for semantic search, RAG, clustering, and similarity comparison.

Input methods:
  - Positional arguments: embed each argument separately
  - Stdin: pipe text to embed
  - Files (-f): embed content of each file

Output formats:
  - json (default): full JSON with model, dimensions, and vectors
  - array: bare JSON array(s) of floats (one per input, for piping)
  - plain: one number per line (single input only)

Examples:
  term-llm embed "What is the meaning of life?"
  term-llm embed "first text" "second text"
  echo "Hello world" | term-llm embed
  term-llm embed -f document.txt
  term-llm embed -f doc1.txt -f doc2.txt
  term-llm embed "hello" -p openai:text-embedding-3-large
  term-llm embed "hello" --dimensions 256
  term-llm embed "hello" --task-type RETRIEVAL_QUERY -p gemini
  term-llm embed --similarity "king" "queen"
  term-llm embed "hello" --format array
  term-llm embed "hello" -o embeddings.json`,
	Args: cobra.ArbitraryArgs,
	RunE: runEmbed,
}

func init() {
	embedCmd.Flags().StringVarP(&embedProvider, "provider", "p", "", "Override embedding provider (gemini, openai, jina, voyage, ollama), optionally with model (e.g., openai:text-embedding-3-large)")
	embedCmd.Flags().StringArrayVarP(&embedFiles, "file", "f", nil, "Input file(s) to embed (can be specified multiple times)")
	embedCmd.Flags().StringVarP(&embedOutput, "output", "o", "", "Write output to file instead of stdout")
	embedCmd.Flags().StringVar(&embedFormat, "format", "json", "Output format: json, array, plain")
	embedCmd.Flags().IntVar(&embedDimensions, "dimensions", 0, "Custom output dimensions (0 = model default)")
	embedCmd.Flags().StringVar(&embedTaskType, "task-type", "", "Gemini task type hint (e.g., RETRIEVAL_QUERY, RETRIEVAL_DOCUMENT, SEMANTIC_SIMILARITY)")
	embedCmd.Flags().BoolVar(&embedSimilarity, "similarity", false, "Compare texts by cosine similarity instead of outputting vectors")

	embedCmd.RegisterFlagCompletionFunc("provider", EmbedProviderFlagCompletion)
	embedCmd.RegisterFlagCompletionFunc("format", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return []string{"json", "array", "plain"}, cobra.ShellCompDirectiveNoFileComp
	})
	embedCmd.RegisterFlagCompletionFunc("task-type", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return []string{
			"RETRIEVAL_QUERY",
			"RETRIEVAL_DOCUMENT",
			"SEMANTIC_SIMILARITY",
			"CLASSIFICATION",
			"CLUSTERING",
			"CODE_RETRIEVAL_QUERY",
			"QUESTION_ANSWERING",
			"FACT_VERIFICATION",
		}, cobra.ShellCompDirectiveNoFileComp
	})

	rootCmd.AddCommand(embedCmd)
}

// EmbedProviderFlagCompletion handles --provider flag completion for embed commands
func EmbedProviderFlagCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	providers := []string{"gemini", "openai", "jina", "voyage", "ollama"}
	var completions []string
	for _, p := range providers {
		if strings.HasPrefix(p, toComplete) {
			completions = append(completions, p)
		}
	}

	if !strings.Contains(toComplete, ":") {
		return completions, cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
	}
	return completions, cobra.ShellCompDirectiveNoFileComp
}

func runEmbed(cmd *cobra.Command, args []string) error {
	// Collect input texts
	var texts []string

	// From positional arguments
	if len(args) > 0 {
		texts = append(texts, args...)
	}

	// From files
	if len(embedFiles) > 0 {
		files, err := input.ReadFiles(embedFiles)
		if err != nil {
			return fmt.Errorf("failed to read files: %w", err)
		}
		for _, f := range files {
			texts = append(texts, f.Content)
		}
	}

	// From stdin
	stdinContent, err := input.ReadStdin()
	if err != nil {
		return fmt.Errorf("failed to read stdin: %w", err)
	}
	if stdinContent = strings.TrimSpace(stdinContent); stdinContent != "" {
		texts = append(texts, stdinContent)
	}

	if len(texts) == 0 {
		return fmt.Errorf("no input text provided: supply as arguments, via stdin, or with -f")
	}

	// Load config
	cfg, err := loadConfigWithSetup()
	if err != nil {
		return err
	}

	// Create provider
	provider, err := embedding.NewEmbeddingProvider(cfg, embedProvider)
	if err != nil {
		return err
	}

	// Build request
	req := embedding.EmbedRequest{
		Texts:      texts,
		Dimensions: embedDimensions,
		TaskType:   embedTaskType,
	}

	// Generate embeddings
	result, err := provider.Embed(cmd.Context(), req)
	if err != nil {
		return err
	}

	// Handle similarity mode
	if embedSimilarity {
		return outputSimilarity(result, texts)
	}

	// Format and output
	output, err := formatEmbedOutput(result, embedFormat)
	if err != nil {
		return err
	}

	if embedOutput != "" {
		if err := os.WriteFile(embedOutput, []byte(output), 0644); err != nil {
			return fmt.Errorf("failed to write output file: %w", err)
		}
		fmt.Fprintf(os.Stderr, "Written to: %s\n", embedOutput)
		return nil
	}

	fmt.Print(output)
	return nil
}

func formatEmbedOutput(result *embedding.EmbeddingResult, format string) (string, error) {
	switch format {
	case "json":
		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return "", fmt.Errorf("failed to marshal JSON: %w", err)
		}
		return string(data) + "\n", nil

	case "array":
		// Output bare JSON array(s), one per line for multiple inputs
		var sb strings.Builder
		for i, emb := range result.Embeddings {
			data, err := json.Marshal(emb.Vector)
			if err != nil {
				return "", fmt.Errorf("failed to marshal vector: %w", err)
			}
			sb.Write(data)
			if i < len(result.Embeddings)-1 {
				sb.WriteString("\n")
			}
		}
		sb.WriteString("\n")
		return sb.String(), nil

	case "plain":
		if len(result.Embeddings) != 1 {
			return "", fmt.Errorf("plain format only supports a single input (got %d)", len(result.Embeddings))
		}
		var sb strings.Builder
		for _, v := range result.Embeddings[0].Vector {
			sb.WriteString(fmt.Sprintf("%g\n", v))
		}
		return sb.String(), nil

	default:
		return "", fmt.Errorf("unknown format %q (valid: json, array, plain)", format)
	}
}

func outputSimilarity(result *embedding.EmbeddingResult, texts []string) error {
	if len(result.Embeddings) < 2 {
		return fmt.Errorf("similarity mode requires at least 2 inputs (got %d)", len(result.Embeddings))
	}

	if len(result.Embeddings) == 2 {
		// Simple pairwise comparison
		sim := embedding.CosineSimilarity(
			result.Embeddings[0].Vector,
			result.Embeddings[1].Vector,
		)
		fmt.Fprintf(os.Stderr, "Model: %s (%dd)\n", result.Model, result.Dimensions)
		fmt.Fprintf(os.Stderr, "  A: %s\n", truncateText(texts[0], 60))
		fmt.Fprintf(os.Stderr, "  B: %s\n", truncateText(texts[1], 60))
		fmt.Printf("%.6f\n", sim)
		return nil
	}

	// Multiple inputs: compare all against the first (query)
	type simResult struct {
		Index      int
		Text       string
		Similarity float64
	}

	fmt.Fprintf(os.Stderr, "Model: %s (%dd)\n", result.Model, result.Dimensions)
	fmt.Fprintf(os.Stderr, "Query: %s\n\n", truncateText(texts[0], 60))

	var results []simResult
	queryVec := result.Embeddings[0].Vector
	for i := 1; i < len(result.Embeddings); i++ {
		sim := embedding.CosineSimilarity(queryVec, result.Embeddings[i].Vector)
		text := ""
		if i < len(texts) {
			text = texts[i]
		}
		results = append(results, simResult{
			Index:      i,
			Text:       text,
			Similarity: sim,
		})
	}

	// Sort by similarity descending
	sort.Slice(results, func(i, j int) bool {
		return results[i].Similarity > results[j].Similarity
	})

	for rank, r := range results {
		fmt.Printf("%d. %.6f  %s\n", rank+1, r.Similarity, truncateText(r.Text, 60))
	}

	return nil
}

func truncateText(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
