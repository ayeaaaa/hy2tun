package ports

import "testing"

func BenchmarkParseRanges(b *testing.B) {
	specs := []string{"2222", "8000-8100", "30000-40000"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := ParseRanges(specs); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPolicyAllocate(b *testing.B) {
	policy, err := NewPolicy(nil, nil, "20000-29999")
	if err != nil {
		b.Fatal(err)
	}
	free := func(int) bool { return true }
	soft := func(int) bool { return false }
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := policy.Allocate(0, free, soft); err != nil {
			b.Fatal(err)
		}
	}
}

func FuzzParseRanges(f *testing.F) {
	for _, seed := range []string{"", "443", "8000-8100", "0", "70000", "9000-8000", "1-65535"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, spec string) {
		_, _ = ParseRanges([]string{spec})
	})
}
