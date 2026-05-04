package itemstate

// Observer is implemented by any component that wants to react to ItemState changes.
//
// OnChange is called by Store.Apply after every successful, non-no-op mutation.
// It is called outside the Store's write lock, so it is safe for observers to call
// Store.Get or Store.Apply from within OnChange without deadlocking.
//
// Ordering note: when two goroutines call Apply concurrently, observers may see
// their changes in a different order than the goroutines submitted them. The Store
// does not guarantee total ordering of concurrent mutations; each Apply is atomic,
// but relative ordering across concurrent callers is undefined.
type Observer interface {
	OnChange(change Change, snapshot Snapshot)
}

// Logger is an optional logging function for Store internals. Accepts a printf-style
// format string and arguments. The default (nil) produces no output.
type Logger func(format string, args ...any)

// ObserverFunc adapts a plain function to the Observer interface.
type ObserverFunc func(change Change, snapshot Snapshot)

// OnChange implements Observer.
func (f ObserverFunc) OnChange(change Change, snapshot Snapshot) {
	f(change, snapshot)
}
