package testdata

import "os"

func syntheticNewPickViolation() {
	_, _ = os.ReadFile("must-be-rejected")
}
