package patchenv

import "testing"

func TestNormalize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "grok trailing stars",
			in:   "*** Begin Patch ***\n*** Add File: a.txt\n+hi\n*** End Patch ***",
			want: "*** Begin Patch\n*** Add File: a.txt\n+hi\n*** End Patch",
		},
		{
			name: "already correct",
			in:   "*** Begin Patch\n*** Update File: a.txt\n@@\n-old\n+new\n*** End Patch",
			want: "*** Begin Patch\n*** Update File: a.txt\n@@\n-old\n+new\n*** End Patch",
		},
		{
			name: "crlf trailing stars",
			in:   "*** Begin Patch ***\r\n*** Add File: a.txt\r\n+hi\r\n*** End Patch ***\r\n",
			want: "*** Begin Patch\r\n*** Add File: a.txt\r\n+hi\r\n*** End Patch\r\n",
		},
		{
			name: "no spaces around stars",
			in:   "***Begin Patch***\n*** Add File: a.txt\n+x\n***End Patch***",
			want: "*** Begin Patch\n*** Add File: a.txt\n+x\n*** End Patch",
		},
		{
			name: "does not touch file content with Patch word",
			in:   "*** Begin Patch\n*** Update File: doc.md\n@@\n-see Patch notes\n+see notes\n*** End Patch",
			want: "*** Begin Patch\n*** Update File: doc.md\n@@\n-see Patch notes\n+see notes\n*** End Patch",
		},
		{
			name: "empty",
			in:   "",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Normalize(tc.in)
			if got != tc.want {
				t.Fatalf("normalize mismatch\n got=%q\nwant=%q", got, tc.want)
			}
		})
	}
}
