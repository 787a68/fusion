package main

import "strings"

func parseParams(config string) map[string]string {
	params := make(map[string]string)
	parts := strings.Split(config, ",")

	for _, part := range parts {
		part = strings.TrimSpace(part)
		keyVal := strings.SplitN(part, "=", 2)
		if len(keyVal) == 2 {
			params[strings.TrimSpace(keyVal[0])] = strings.TrimSpace(keyVal[1])
		}
	}

	return params
}
