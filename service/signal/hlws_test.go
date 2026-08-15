package signal

import (
	"encoding/json"
	"testing"
)

func TestFlexFloatUnmarshal(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want float64
	}{
		{"numeric string", `"62973.0"`, 62973.0},
		{"number", `62973`, 62973.0},
		{"number with decimals", `62956.5`, 62956.5},
		{"string with spaces", `" 0.00017 "`, 0.00017},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got flexFloat
			if err := json.Unmarshal([]byte(tc.in), &got); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.in, err)
			}
			if float64(got) != tc.want {
				t.Fatalf("unmarshal %s = %v, want %v", tc.in, float64(got), tc.want)
			}
		})
	}
}

func TestFlexFloatUnmarshalInvalid(t *testing.T) {
	var got flexFloat
	if err := json.Unmarshal([]byte(`"abc"`), &got); err == nil {
		t.Fatalf("expected error for non-numeric string, got %v", float64(got))
	}
}

func TestFlexFloatInFrames(t *testing.T) {
	raw := `{"channel":"trades","data":[{"coin":"BTC","side":"B","px":"62973.0","sz":"0.00017"}]}`
	var fr wsTradesFrame
	if err := json.Unmarshal([]byte(raw), &fr); err != nil {
		t.Fatalf("unmarshal trades frame: %v", err)
	}
	if len(fr.Data) != 1 {
		t.Fatalf("expected 1 trade, got %d", len(fr.Data))
	}
	tr := fr.Data[0]
	if float64(tr.Px) != 62973.0 || float64(tr.Sz) != 0.00017 {
		t.Fatalf("trade px/sz = %v/%v, want 62973.0/0.00017", float64(tr.Px), float64(tr.Sz))
	}

	rawBook := `{"channel":"l2Book","data":{"coin":"BTC","levels":[[{"px":"62956.0","sz":"15.00"}],[{"px":"62957.0","sz":"3.2"}]],"time":1}}`
	var bf wsBookFrame
	if err := json.Unmarshal([]byte(rawBook), &bf); err != nil {
		t.Fatalf("unmarshal book frame: %v", err)
	}
	if len(bf.Data.Levels) != 2 {
		t.Fatalf("expected 2 level sides, got %d", len(bf.Data.Levels))
	}
	bid := bf.Data.Levels[0][0]
	ask := bf.Data.Levels[1][0]
	if float64(bid.Px) != 62956.0 || float64(bid.Sz) != 15.00 {
		t.Fatalf("bid px/sz = %v/%v, want 62956.0/15.0", float64(bid.Px), float64(bid.Sz))
	}
	if float64(ask.Px) != 62957.0 || float64(ask.Sz) != 3.2 {
		t.Fatalf("ask px/sz = %v/%v, want 62957.0/3.2", float64(ask.Px), float64(ask.Sz))
	}
}
