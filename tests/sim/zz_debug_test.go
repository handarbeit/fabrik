package sim

import "testing"

func dumpMutations(t *testing.T, env *Env) {
	for _, e := range env.Sim.Log().Mutations() {
		t.Logf("MUT %d %s %s", e.Seq, e.Method, e.Args.String())
	}
}
