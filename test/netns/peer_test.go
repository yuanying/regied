package netns

import "testing"

// whoami の応答は 1 回の読み取りで何行返ってくるか分からない。socat の
// バージョンによっては、送信側が EOF で空のデータグラムを足し、それにも
// 応答が返るため、同じ観測が 2 つ並ぶ（Ubuntu 26.04 の socat 1.8 で実際に
// 起きた）。何回に分かれて届いても同じ判定になること。
func TestParsePeer(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		want    peerAddr
		wantErr bool
	}{
		{
			name: "1 行だけ返る",
			out:  "198.51.100.2:40001\n",
			want: peerAddr{IP: "198.51.100.2", Port: 40001},
		},
		{
			name: "同じ観測が 2 行返る",
			out:  "198.51.100.2:40001\n198.51.100.2:40001",
			want: peerAddr{IP: "198.51.100.2", Port: 40001},
		},
		{
			name: "同じ観測が 3 行、末尾に改行がある",
			out:  "192.0.2.1:1234\n192.0.2.1:1234\n192.0.2.1:1234\n",
			want: peerAddr{IP: "192.0.2.1", Port: 1234},
		},
		{
			name: "前後に空白が付く",
			out:  "  198.51.100.2:40001  \n",
			want: peerAddr{IP: "198.51.100.2", Port: 40001},
		},
		{
			name:    "応答がない",
			out:     "",
			wantErr: true,
		},
		{
			name:    "空白だけ",
			out:     " \n\n",
			wantErr: true,
		},
		{
			name:    "ポートが無い",
			out:     "198.51.100.2\n",
			wantErr: true,
		},
		{
			name:    "ポートが数字でない",
			out:     "198.51.100.2:ssh\n",
			wantErr: true,
		},
		{
			// 観測が食い違うなら、どちらを信じてよいか分からない。
			// 黙って片方を採ると、NAT のマッピングを読み違える。
			name:    "観測が食い違う",
			out:     "198.51.100.2:40001\n198.51.100.2:40002",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePeer(tt.out)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("失敗するはずが %v を返した", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("読めるはずが失敗した: %v", err)
			}
			if got != tt.want {
				t.Errorf("読み取りが違う: got %v, want %v", got, tt.want)
			}
		})
	}
}
