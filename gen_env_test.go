package ics

import "os"

func genLookupEnv(key string) (string, bool) { return os.LookupEnv(key) }

func genEnvInt(key string) int {
	v, ok := genLookupEnv(key)
	if !ok {
		return 0
	}
	n := 0
	for _, r := range v {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}
