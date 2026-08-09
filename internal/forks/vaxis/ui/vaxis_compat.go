package ui

import "github.com/memcode-ai/memcode/internal/forks/vaxis"

func vaxisCharacters(s string) []Character {
	return vaxis.Characters(s)
}

func vaxisEncodeCells(cells []Cell) string {
	return vaxis.EncodeCells(cells)
}
