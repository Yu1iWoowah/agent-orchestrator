package operatornative

import "sync"

// Owner holds a kernel-backed watch of the original app process. An absent or
// failed watch never proves death and therefore never unlocks tokenless attach.
type Owner struct {
	mu      sync.Mutex
	exited  func() (bool, error)
	release func() error
	closed  bool
}

func (o *Owner) ended() bool {
	if o == nil {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed || o.exited == nil {
		return false
	}
	ended, err := o.exited()
	return err == nil && ended
}

// Close releases the bootstrap watch; closing it does not prove owner exit.
func (o *Owner) Close() error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	o.closed = true
	if o.release != nil {
		return o.release()
	}
	return nil
}
