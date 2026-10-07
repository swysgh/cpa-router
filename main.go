package main

// main is required for the c-shared build mode even though the plugin is loaded
// via the exported C ABI symbols. It is never called at runtime.
func main() {}
