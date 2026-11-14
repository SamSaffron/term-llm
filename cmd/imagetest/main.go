// Standalone test binary to debug image rendering with streaming markdown
package main

import (
	"flag"
	"fmt"
	goimage "image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"time"

	"github.com/BourgeoisBear/rasterm"
	"github.com/term-llm/term-llm/internal/image"
	"github.com/term-llm/term-llm/internal/ui"
	_ "golang.org/x/image/webp"
)

func main() {
	debug := flag.Bool("debug", false, "Show debug information about placeholder")
	direct := flag.Bool("direct", false, "Use direct display mode (rasterm) instead of Unicode placeholders")
	raw := flag.Bool("raw", false, "Use raw rasterm Kitty protocol directly (no wrapper)")
	rawIterm := flag.Bool("raw-iterm", false, "Use raw rasterm iTerm protocol directly")
	noStream := flag.Bool("no-stream", false, "Skip streaming simulation")
	noCache := flag.Bool("no-cache", false, "Skip cached render test")
	flag.Parse()

	args := flag.Args()
	if len(args) < 1 {
		fmt.Println("Usage: imagetest [-debug] [-direct] [-raw] [-raw-iterm] <image_path>")
		fmt.Println("Example: imagetest /path/to/cat.png")
		fmt.Println("         imagetest -debug /path/to/cat.png")
		fmt.Println("         imagetest -direct /path/to/cat.png  # use our DisplayImage wrapper")
		fmt.Println("         imagetest -raw /path/to/cat.png     # use rasterm Kitty directly")
		fmt.Println("         imagetest -raw-iterm /path/to/cat.png # use rasterm iTerm directly")
		os.Exit(1)
	}

	imagePath := args[0]

	// Check if file exists
	if _, err := os.Stat(imagePath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: file not found: %s\n", imagePath)
		os.Exit(1)
	}

	fmt.Println("=== Image Rendering Test ===")
	fmt.Println()

	// Detect capability
	cap := image.DetectCapability()
	fmt.Printf("Detected capability: %s\n", cap)
	fmt.Printf("Mode: %s\n", map[bool]string{true: "direct (rasterm)", false: "unicode placeholders"}[*direct])
	fmt.Println()

	// Step 1: Render a tool completion indicator
	fmt.Printf("%s Generated image\n", ui.SuccessCircle())
	fmt.Println()

	// Step 2: Render the image
	fmt.Println("--- Rendering image ---")

	var rendered string
	if *raw {
		// Use rasterm directly - no wrapper code at all
		f, err := os.Open(imagePath)
		if err != nil {
			fmt.Printf("Error opening file: %v\n", err)
			os.Exit(1)
		}
		img, _, err := goimage.Decode(f)
		f.Close()
		if err != nil {
			fmt.Printf("Error decoding image: %v\n", err)
			os.Exit(1)
		}

		// Test raw rasterm - Kitty protocol
		fmt.Println("[RAW] Using rasterm.KittyWriteImage directly:")
		err = rasterm.KittyWriteImage(os.Stdout, img, rasterm.KittyImgOpts{})
		if err != nil {
			fmt.Printf("Error: %v\n", err)
		}
		fmt.Print("\r\n") // CR+LF after image
		rendered = "(raw rasterm Kitty mode)"
	} else if *rawIterm {
		// Use rasterm iTerm protocol directly
		f, err := os.Open(imagePath)
		if err != nil {
			fmt.Printf("Error opening file: %v\n", err)
			os.Exit(1)
		}
		img, _, err := goimage.Decode(f)
		f.Close()
		if err != nil {
			fmt.Printf("Error decoding image: %v\n", err)
			os.Exit(1)
		}

		fmt.Println("[RAW-ITERM] Using rasterm.ItermWriteImage directly:")
		err = rasterm.ItermWriteImage(os.Stdout, img)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
		}
		fmt.Print("\r\n") // CR+LF after image
		rendered = "(raw rasterm iTerm mode)"
	} else if *direct {
		// Use direct display via rasterm (original method)
		err := image.DisplayImage(imagePath)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
		}
		rendered = "(direct mode - no placeholder)"
	} else {
		// Use Unicode placeholder method
		rendered = ui.RenderInlineImage(imagePath)
		if rendered == "" {
			fmt.Println("(no image rendered - terminal may not support images)")
		} else {
			if *debug {
				fmt.Printf("[DEBUG] Placeholder length: %d bytes\n", len(rendered))
				// Show first part as hex to see what we're dealing with
				debugLen := min(100, len(rendered))
				fmt.Printf("[DEBUG] First %d bytes (hex): ", debugLen)
				for i := 0; i < debugLen; i++ {
					fmt.Printf("%02x ", rendered[i])
				}
				fmt.Println()
				fmt.Println()
			}
			fmt.Print(rendered)
			fmt.Print("\r\n") // CR+LF to reset cursor
		}
	}
	fmt.Println("--- End image ---")
	fmt.Println()

	// Step 3: Stream some markdown-like text
	if !*noStream {
		fmt.Println("--- Streaming text ---")
		text := "Here is the image I generated for you. It shows a beautiful cat sitting on a rainbow. The colors are vibrant and the composition is lovely."
		for _, char := range text {
			fmt.Print(string(char))
			time.Sleep(20 * time.Millisecond)
		}
		fmt.Println()
		fmt.Println("--- End streaming ---")
		fmt.Println()
	}

	// Step 4: Try rendering the same image again (should use cache)
	if !*direct && !*raw && !*rawIterm && !*noCache {
		fmt.Println("--- Rendering same image again (cached) ---")
		rendered2 := ui.RenderInlineImage(imagePath)
		if rendered2 == "" {
			fmt.Println("(no image rendered)")
		} else {
			if *debug {
				fmt.Printf("[DEBUG] Cached placeholder length: %d bytes\n", len(rendered2))
				fmt.Printf("[DEBUG] Same as original: %v\n", rendered == rendered2)
				fmt.Println()
			}
			fmt.Print(rendered2)
			fmt.Print("\r\n")
		}
		fmt.Println("--- End cached image ---")
		fmt.Println()
	}

	fmt.Println("=== Test Complete ===")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
