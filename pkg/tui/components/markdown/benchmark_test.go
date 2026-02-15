
package markdown

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
)

const (
	shortContent = `This is a short paragraph of text.`
	longContent  = `
# This is a heading

This is a paragraph with some **bold** and *italic* text.

- This is a list item
- This is another list item

` + "```go" + `
package main

import "fmt"

func main() {
    fmt.Println("Hello, world!")
}
` + "```" + `
`
)

func BenchmarkFastRenderer(b *testing.B) {
	renderer, err := NewFastRenderer(lipgloss.DefaultRenderer())
	if err != nil {
		b.Fatalf("failed to create new fast renderer: %v", err)
	}

	b.Run("short content", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, err := renderer.Render(shortContent, 80)
			if err != nil {
				b.Fatalf("failed to render markdown: %v", err)
			}
		}
	})

	b.Run("long content", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, err := renderer.Render(longContent, 80)
			if err != nil {
				b.Fatalf("failed to render markdown: %v", err)
			}
		}
	})
}
