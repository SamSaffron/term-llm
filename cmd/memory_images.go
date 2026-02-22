package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	memorydb "github.com/samsaffron/term-llm/internal/memory"
	"github.com/spf13/cobra"
)

var (
	memoryImagesLimit  int
	memoryImagesOffset int
	memoryImagesJSON   bool
)

var memoryImagesCmd = &cobra.Command{
	Use:   "images",
	Short: "Browse and search generated images",
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Help()
	},
}

var memoryImagesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List generated images (newest first)",
	RunE:  runMemoryImagesList,
}

var memoryImagesSearchCmd = &cobra.Command{
	Use:   "search <query>",
	Short: "Search generated images by prompt",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runMemoryImagesSearch,
}

func init() {
	memoryImagesListCmd.Flags().IntVar(&memoryImagesLimit, "limit", 20, "Maximum number of results")
	memoryImagesListCmd.Flags().IntVar(&memoryImagesOffset, "offset", 0, "Offset for pagination")
	memoryImagesListCmd.Flags().BoolVar(&memoryImagesJSON, "json", false, "Output as JSON")

	memoryImagesSearchCmd.Flags().IntVar(&memoryImagesLimit, "limit", 10, "Maximum number of results")
	memoryImagesSearchCmd.Flags().BoolVar(&memoryImagesJSON, "json", false, "Output as JSON")

	memoryImagesCmd.AddCommand(memoryImagesListCmd)
	memoryImagesCmd.AddCommand(memoryImagesSearchCmd)
}

func runMemoryImagesList(cmd *cobra.Command, args []string) error {
	store, err := openMemoryStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	images, err := store.ListImages(ctx, memorydb.ImageListOptions{
		Agent:  memoryAgent,
		Limit:  memoryImagesLimit,
		Offset: memoryImagesOffset,
	})
	if err != nil {
		return err
	}

	return printImages(images, memoryImagesJSON)
}

func runMemoryImagesSearch(cmd *cobra.Command, args []string) error {
	store, err := openMemoryStore()
	if err != nil {
		return err
	}
	defer store.Close()

	query := strings.TrimSpace(strings.Join(args, " "))
	ctx := context.Background()
	images, err := store.SearchImages(ctx, query, memoryAgent, memoryImagesLimit)
	if err != nil {
		return err
	}

	return printImages(images, memoryImagesJSON)
}

func printImages(images []memorydb.ImageRecord, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(images)
	}

	if len(images) == 0 {
		fmt.Println("No images found.")
		return nil
	}

	for _, img := range images {
		age := formatRelativeTime(img.CreatedAt)
		prompt := img.Prompt
		if len(prompt) > 60 {
			prompt = prompt[:57] + "..."
		}
		sizeStr := ""
		if img.FileSize > 0 {
			sizeStr = fmt.Sprintf(" (%s)", formatBytes(int64(img.FileSize)))
		}
		dims := ""
		if img.Width > 0 && img.Height > 0 {
			dims = fmt.Sprintf(" %dx%d", img.Width, img.Height)
		}
		fmt.Printf("[%s] %s\n  %s%s%s  %s\n\n",
			age,
			prompt,
			img.OutputPath,
			sizeStr,
			dims,
			img.Provider,
		)
	}
	return nil
}
