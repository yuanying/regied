package netns

import "testing"

// There is no telling how many lines a single read of the whoami response returns. With
// some socat versions the sending side appends an empty datagram on EOF, that one is
// answered too, and two identical observations end up side by side (this actually
// happened with socat 1.8 on Ubuntu 26.04). The verdict must be the same however many
// pieces the response arrives in.
func TestParsePeer(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		want    peerAddr
		wantErr bool
	}{
		{
			name: "a single line comes back",
			out:  "198.51.100.2:40001\n",
			want: peerAddr{IP: "198.51.100.2", Port: 40001},
		},
		{
			name: "the same observation comes back twice",
			out:  "198.51.100.2:40001\n198.51.100.2:40001",
			want: peerAddr{IP: "198.51.100.2", Port: 40001},
		},
		{
			name: "the same observation three times, with a trailing newline",
			out:  "192.0.2.1:1234\n192.0.2.1:1234\n192.0.2.1:1234\n",
			want: peerAddr{IP: "192.0.2.1", Port: 1234},
		},
		{
			name: "surrounded by whitespace",
			out:  "  198.51.100.2:40001  \n",
			want: peerAddr{IP: "198.51.100.2", Port: 40001},
		},
		{
			name:    "no response at all",
			out:     "",
			wantErr: true,
		},
		{
			name:    "whitespace only",
			out:     " \n\n",
			wantErr: true,
		},
		{
			name:    "no port",
			out:     "198.51.100.2\n",
			wantErr: true,
		},
		{
			name:    "the port is not a number",
			out:     "198.51.100.2:ssh\n",
			wantErr: true,
		},
		{
			// If the observations disagree there is no telling which to believe.
			// Silently picking one of them misreads the NAT mapping.
			name:    "the observations disagree",
			out:     "198.51.100.2:40001\n198.51.100.2:40002",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePeer(tt.out)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected a failure, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected this to be readable, but it failed: %v", err)
			}
			if got != tt.want {
				t.Errorf("read the wrong value: got %v, want %v", got, tt.want)
			}
		})
	}
}
