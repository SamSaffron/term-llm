package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	memorydb "github.com/samsaffron/term-llm/internal/memory"
)

// ImagesOptions configures `memory images list` and `memory images search`.
type ImagesOptions struct {
	Env
	Limit  int
	Offset int
	JSON   bool
}

// ImagesList lists generated images, newest first.
func ImagesList(opts ImagesOptions) error {
	env := opts.Env
	store, err := env.OpenStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	images, err := store.ListImages(ctx, memorydb.ImageListOptions{
		Agent:  env.Agent,
		Limit:  opts.Limit,
		Offset: opts.Offset,
	})
	if err != nil {
		return err
	}

	return printImages(env, images, opts.JSON)
}

// ImagesSearch searches generated images by prompt.
func ImagesSearch(opts ImagesOptions, args []string) error {
	env := opts.Env
	store, err := env.OpenStore()
	if err != nil {
		return err
	}
	defer store.Close()

	query := strings.TrimSpace(strings.Join(args, " "))
	ctx := context.Background()
	images, err := store.SearchImages(ctx, query, env.Agent, opts.Limit)
	if err != nil {
		return err
	}

	return printImages(env, images, opts.JSON)
}

func printImages(env Env, images []memorydb.ImageRecord, asJSON bool) error {
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
		age := env.FormatRelativeTime(img.CreatedAt)
		prompt := img.Prompt
		if len(prompt) > 60 {
			prompt = prompt[:57] + "..."
		}
		sizeStr := ""
		if img.FileSize > 0 {
			sizeStr = fmt.Sprintf(" (%s)", env.FormatBytes(int64(img.FileSize)))
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
