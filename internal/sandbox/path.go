package sandbox

import "strings"

func ValidTransferPath(value string) bool {
	if len(value) == 0 || len(value) > 512 || strings.Contains(value, "\\") || strings.Contains(value, "//") {
		return false
	}
	matched := ""
	for _, prefix := range []string{"/workspace/src/", "/workspace/artifacts/", "/workspace/tmp/"} {
		if strings.HasPrefix(value, prefix) {
			matched = prefix
			break
		}
	}
	if matched == "" {
		return false
	}
	for _, segment := range strings.Split(strings.TrimPrefix(value, matched), "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
		for _, character := range []byte(segment) {
			if !(character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '.' || character == '_' || character == '-') {
				return false
			}
		}
	}
	return true
}
