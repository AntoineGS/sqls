package queryparams

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBindPreservesIntegerPrecisionAndRepeatedValues(t *testing.T) {
	batch, err := Compile("SELECT :ID, :id, :EMPTY, :N FROM T", 3)
	if err != nil {
		t.Fatal(err)
	}
	args, err := Bind(batch, []Value{
		{Name: "id", Type: "integer", Value: "9007199254740993"},
		{Name: "EMPTY", Type: "text", Value: ""},
		{Name: "N", Type: "null", Value: ""},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []any{int64(9007199254740993), int64(9007199254740993), "", nil}
	if !reflect.DeepEqual(args[0], want) {
		t.Fatalf("got %#v", args[0])
	}
}

func TestConvertInteger(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    int64
		wantErr bool
	}{
		{name: "min", value: "-9223372036854775808", want: -9223372036854775808},
		{name: "max", value: "9223372036854775807", want: 9223372036854775807},
		{name: "overflow", value: "9223372036854775808", wantErr: true},
		{name: "negative overflow", value: "-9223372036854775809", wantErr: true},
		{name: "whitespace trimmed", value: "  42  ", want: 42},
		{name: "hex rejected", value: "0x2A", wantErr: true},
		{name: "leading plus", value: "+5", want: 5},
		{name: "decimal point rejected", value: "5.0", wantErr: true},
		{name: "empty rejected", value: "", wantErr: true},
		{name: "scientific rejected", value: "5e2", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Convert(Value{Name: "N", Type: "integer", Value: tt.value})
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Convert() error = nil, want error for %q", tt.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("Convert() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("Convert() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestConvertNumber(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    float64
		wantErr bool
	}{
		{name: "negative", value: "-3.5", want: -3.5},
		{name: "scientific", value: "1.5e10", want: 1.5e10},
		{name: "negative scientific", value: "-1.5e-10", want: -1.5e-10},
		{name: "whitespace trimmed", value: "  2.5  ", want: 2.5},
		{name: "NaN rejected", value: "NaN", wantErr: true},
		{name: "Inf rejected", value: "Inf", wantErr: true},
		{name: "+Inf rejected", value: "+Inf", wantErr: true},
		{name: "overflow rejected", value: "1e400", wantErr: true},
		{name: "hex float rejected", value: "0x1p3", wantErr: true},
		{name: "empty rejected", value: "", wantErr: true},
		{name: "trailing garbage rejected", value: "1.5abc", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Convert(Value{Name: "N", Type: "number", Value: tt.value})
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Convert() error = nil, want error for %q", tt.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("Convert() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("Convert() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestConvertDate(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    time.Time
		wantErr bool
	}{
		{name: "leap day valid", value: "2024-02-29", want: time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC)},
		{name: "leap day invalid", value: "2023-02-29", wantErr: true},
		{name: "whitespace trimmed", value: "  2023-01-02  ", want: time.Date(2023, 1, 2, 0, 0, 0, 0, time.UTC)},
		{name: "month out of range", value: "2023-13-01", wantErr: true},
		{name: "single digit month rejected", value: "2023-1-02", wantErr: true},
		{name: "empty rejected", value: "", wantErr: true},
		{name: "trailing text rejected", value: "2023-01-02T00:00:00", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Convert(Value{Name: "N", Type: "date", Value: tt.value})
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Convert() error = nil, want error for %q", tt.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("Convert() error = %v", err)
			}
			gotTime, ok := got.(time.Time)
			if !ok {
				t.Fatalf("Convert() = %#v, want time.Time", got)
			}
			if !gotTime.Equal(tt.want) || gotTime.Location() != time.UTC {
				t.Fatalf("Convert() = %v, want %v", gotTime, tt.want)
			}
		})
	}
}

func TestConvertTimestamp(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    time.Time
		wantErr bool
	}{
		{
			name:  "no fraction",
			value: "2023-01-02 15:04:05",
			want:  time.Date(2023, 1, 2, 15, 4, 5, 0, time.UTC),
		},
		{
			name:  "fraction",
			value: "2023-01-02 15:04:05.123",
			want:  time.Date(2023, 1, 2, 15, 4, 5, 123000000, time.UTC),
		},
		{
			name:  "nine digit fraction",
			value: "2023-01-02 15:04:05.123456789",
			want:  time.Date(2023, 1, 2, 15, 4, 5, 123456789, time.UTC),
		},
		{name: "ten digit fraction rejected", value: "2023-01-02 15:04:05.1234567891", wantErr: true},
		{name: "timezone Z rejected", value: "2023-01-02 15:04:05Z", wantErr: true},
		{name: "timezone offset rejected", value: "2023-01-02 15:04:05+02:00", wantErr: true},
		{name: "hour out of range", value: "2023-01-02 24:00:00", wantErr: true},
		{name: "empty rejected", value: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Convert(Value{Name: "N", Type: "timestamp", Value: tt.value})
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Convert() error = nil, want error for %q", tt.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("Convert() error = %v", err)
			}
			gotTime, ok := got.(time.Time)
			if !ok {
				t.Fatalf("Convert() = %#v, want time.Time", got)
			}
			if !gotTime.Equal(tt.want) || gotTime.Location() != time.UTC {
				t.Fatalf("Convert() = %v, want %v", gotTime, tt.want)
			}
		})
	}
}

func TestConvertBoolean(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    bool
		wantErr bool
	}{
		{name: "true", value: "true", want: true},
		{name: "false", value: "false", want: false},
		{name: "whitespace trimmed", value: "  true  ", want: true},
		{name: "capitalized rejected", value: "True", wantErr: true},
		{name: "numeric rejected", value: "1", wantErr: true},
		{name: "empty rejected", value: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Convert(Value{Name: "N", Type: "boolean", Value: tt.value})
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Convert() error = nil, want error for %q", tt.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("Convert() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("Convert() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestConvertText(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "empty", value: ""},
		{name: "apostrophe", value: "O'Brien"},
		{name: "newline", value: "line1\nline2"},
		{name: "leading zeros", value: "007"},
		{name: "surrounding whitespace preserved", value: "  padded  "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Convert(Value{Name: "N", Type: "text", Value: tt.value})
			if err != nil {
				t.Fatalf("Convert() error = %v", err)
			}
			if got != tt.value {
				t.Fatalf("Convert() = %#v, want %#v", got, tt.value)
			}
		})
	}
}

func TestConvertNull(t *testing.T) {
	got, err := Convert(Value{Name: "N", Type: "null", Value: ""})
	if err != nil {
		t.Fatalf("Convert() error = %v", err)
	}
	if got != nil {
		t.Fatalf("Convert() = %#v, want nil", got)
	}

	if _, err := Convert(Value{Name: "N", Type: "null", Value: "anything"}); err == nil {
		t.Fatal("Convert() error = nil, want error for nonempty NULL value")
	}
}

func TestConvertRejectsInvalidType(t *testing.T) {
	if _, err := Convert(Value{Name: "N", Type: "money", Value: "5"}); err == nil {
		t.Fatal("Convert() error = nil, want error for unsupported type")
	}
}

func TestConvertErrorsDoNotEchoInputValue(t *testing.T) {
	secret := "sk-super-secret-do-not-log-1234567890"
	tests := []Value{
		{Name: "N", Type: "integer", Value: secret},
		{Name: "N", Type: "number", Value: secret},
		{Name: "N", Type: "date", Value: secret},
		{Name: "N", Type: "timestamp", Value: secret},
		{Name: "N", Type: "boolean", Value: secret},
		{Name: "N", Type: "null", Value: secret},
	}
	for _, tt := range tests {
		_, err := Convert(tt)
		if err == nil {
			t.Fatalf("Convert() error = nil for type %q, want error", tt.Type)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("Convert() error %q leaks the input value", err.Error())
		}
	}
}

func TestBindRejectsMissingParameterValue(t *testing.T) {
	batch, err := Compile("SELECT :A, :B FROM T", 3)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Bind(batch, []Value{{Name: "A", Type: "text", Value: "x"}})
	if err == nil {
		t.Fatal("Bind() error = nil, want error for missing parameter value")
	}
}

func TestBindRejectsExtraParameterValue(t *testing.T) {
	batch, err := Compile("SELECT :A FROM T", 3)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Bind(batch, []Value{
		{Name: "A", Type: "text", Value: "x"},
		{Name: "B", Type: "text", Value: "y"},
	})
	if err == nil {
		t.Fatal("Bind() error = nil, want error for extra parameter value")
	}
}

func TestBindRejectsDuplicateCaseFoldedName(t *testing.T) {
	batch, err := Compile("SELECT :A FROM T", 3)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Bind(batch, []Value{
		{Name: "A", Type: "text", Value: "x"},
		{Name: "a", Type: "text", Value: "y"},
	})
	if err == nil {
		t.Fatal("Bind() error = nil, want error for duplicate case-folded name")
	}
}

func TestBindInvalidValueInSecondStatementYieldsNoArgs(t *testing.T) {
	batch, err := Compile("SELECT :A FROM T; SELECT :B FROM U", 3)
	if err != nil {
		t.Fatal(err)
	}
	args, err := Bind(batch, []Value{
		{Name: "A", Type: "text", Value: "ok"},
		{Name: "B", Type: "integer", Value: "not-an-integer"},
	})
	if err == nil {
		t.Fatal("Bind() error = nil, want error from the invalid second-statement value")
	}
	if args != nil {
		t.Fatalf("Bind() args = %#v, want nil when any value is invalid", args)
	}
}

func TestBindErrorsDoNotLeakInputValue(t *testing.T) {
	batch, err := Compile("SELECT :A FROM T", 3)
	if err != nil {
		t.Fatal(err)
	}
	secret := "sk-super-secret-do-not-log-1234567890"
	_, err = Bind(batch, []Value{{Name: "A", Type: "integer", Value: secret}})
	if err == nil {
		t.Fatal("Bind() error = nil, want error for invalid integer value")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("Bind() error %q leaks the input value", err.Error())
	}
}
