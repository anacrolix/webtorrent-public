package wtpub

import (
	"regexp"
	"strconv"
)

var bitCometPaddingFile = regexp.MustCompile(`padding_file_\d+_.+BitComet.*0\.85.+`)

func IsPaddingPath(path []string) bool {
	if len(path) == 2 && path[0] == ".pad" {
		_, err := strconv.Atoi(path[1])
		if err == nil {
			return true
		}
	}
	if len(path) == 0 {
		return false
	}
	return bitCometPaddingFile.MatchString(path[len(path)-1])
}
