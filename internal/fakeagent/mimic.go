package main

import (
	"fmt"
	"os"
	"strings"
)

const invalidModel = "invalid-model"

// rejectModel exits the way the real CLI does when --model names a model it
// does not offer: after initialize, without answering authenticate, and with
// the whole model list on stderr.
func rejectModel() {
	names := make([]string, 300)
	for i := range names {
		names[i] = fmt.Sprintf("fake-model-%03d", i+1)
	}
	fmt.Fprintf(os.Stderr, "Cannot use this model: %s. Available models: %s\n", invalidModel, strings.Join(names, ", "))
	os.Exit(1)
}
