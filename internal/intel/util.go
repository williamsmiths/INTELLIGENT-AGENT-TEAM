package intel

import "regexp"

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// isUUID keeps malformed ids away from uuid columns: they are simply not found.
func isUUID(id string) bool { return uuidRe.MatchString(id) }

// Clip collapses whitespace and cuts s to n characters (exported for the API texts).
func Clip(s string, n int) string { return clip(s, n) }
