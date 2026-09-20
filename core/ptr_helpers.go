package core

// setFloat32 returns a pointer to the given float32 value.
// Used for setting optional float32 fields on config structs.
func setFloat32(v float32) *float32 { return &v }
