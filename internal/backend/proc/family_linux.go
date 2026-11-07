package proc

// On Linux the token alone finds the descendants that left the group, because
// a user can read the environments of its own processes.
type family struct{}

func watchFamily(int) *family { return nil }

func (*family) owns(int) bool { return false }

func (*family) close() {}
