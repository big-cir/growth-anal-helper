package ga4

import "growth-lab/internal/sensitive"

// secretShape returns the matched shape, or "".
func secretShape(text string) string { return sensitive.SecretShape(text) }
