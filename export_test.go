package alsa

// XrunRecoverForTest exposes xrunRecover to white-box tests.
func XrunRecoverForTest(p *PCM, err error) error {
	return p.xrunRecover(err)
}
