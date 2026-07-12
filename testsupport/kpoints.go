package testsupport

import "errors"

var ErrInjectedCrash = errors.New("injected crash")

type KPointRegistry struct{ points map[string]struct{} }

func NewKPointRegistry(names ...string) *KPointRegistry {
	r := &KPointRegistry{points: map[string]struct{}{}}
	for _, n := range names {
		r.points[n] = struct{}{}
	}
	return r
}
func (r *KPointRegistry) Has(name string) bool { _, ok := r.points[name]; return ok }

type CrashController struct {
	registry *KPointRegistry
	armed    map[string]bool
}

func NewCrashController(r *KPointRegistry, names ...string) *CrashController {
	c := &CrashController{registry: r, armed: map[string]bool{}}
	for _, n := range names {
		c.armed[n] = true
	}
	return c
}
func (c *CrashController) Hit(name string) error {
	if c.registry == nil || !c.registry.Has(name) {
		return errors.New("unregistered k-point")
	}
	if c.armed[name] {
		delete(c.armed, name)
		return ErrInjectedCrash
	}
	return nil
}
