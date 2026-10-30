package pipeline

// testHooks stop or fail at fixed points in tests.
var testHooks struct {
	initStep func(step string) error
}

func hook(step string) error {
	if h := testHooks.initStep; h != nil {
		return h(step)
	}
	return nil
}
