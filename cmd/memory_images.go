package cmd

import (
	appmemory "github.com/samsaffron/term-llm/internal/app/memory"
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

func memoryImagesOptions() appmemory.ImagesOptions {
	return appmemory.ImagesOptions{
		Env:    memoryEnv(),
		Limit:  memoryImagesLimit,
		Offset: memoryImagesOffset,
		JSON:   memoryImagesJSON,
	}
}

func runMemoryImagesList(cmd *cobra.Command, args []string) error {
	return appmemory.ImagesList(memoryImagesOptions())
}

func runMemoryImagesSearch(cmd *cobra.Command, args []string) error {
	return appmemory.ImagesSearch(memoryImagesOptions(), args)
}
