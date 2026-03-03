package main

func maskAPIKey(key string) string {
	if len(key) < 10 {
		return "***"
	}
	return key[:4] + "..." + key[len(key)-4:]
}
