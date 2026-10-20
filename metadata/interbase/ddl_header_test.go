package ibmeta

import "testing"

func TestFormatDDLHeader(t *testing.T) {
	tests := []struct {
		name, header, want string
		dialect            int
	}{
		{
			name: "procedure parameters one per line",
			header: `CREATE PROCEDURE "CASH_U_LOG_01" ("VCUSTID" VARCHAR(8) CHARACTER SET NONE, "VCASHID" VARCHAR(4) CHARACTER SET NONE) ` +
				`RETURNS ("CUSTID" VARCHAR(8) CHARACTER SET NONE, "LOGINS" INTEGER) AS`,
			want: "CREATE PROCEDURE \"CASH_U_LOG_01\" (\n  \"VCUSTID\" VARCHAR(8) CHARACTER SET NONE,\n  \"VCASHID\" VARCHAR(4) CHARACTER SET NONE\n" +
				") RETURNS (\n  \"CUSTID\" VARCHAR(8) CHARACTER SET NONE,\n  \"LOGINS\" INTEGER\n) AS",
			dialect: 3,
		},
		{
			name:    "dialect 1 view columns",
			header:  `CREATE VIEW V_CUST (ID, NAME) AS`,
			want:    "CREATE VIEW V_CUST (\n  ID,\n  NAME\n) AS",
			dialect: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := formatDDLHeader(tt.header, tt.dialect)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("formatDDLHeader =\n%s\nwant\n%s", got, tt.want)
			}
			again, err := formatDDLHeader(got, tt.dialect)
			if err != nil {
				t.Fatal(err)
			}
			if again != got {
				t.Fatalf("formatting the formatted header again changed it:\n%s", again)
			}
		})
	}
}
