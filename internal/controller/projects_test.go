package controller

import (
	"testing"

	"lira/internal/model"
)

func TestValidateProject(t *testing.T) {
	cases := []struct {
		name    string
		in      model.ProjectInput
		wantErr bool
	}{
		{"valid save", model.ProjectInput{Name: "Kitchen", FinanceMethod: "save"}, false},
		{"valid loan with term", model.ProjectInput{Name: "Solar", FinanceMethod: "loan", LoanTermMonths: 60}, false},
		{"empty name", model.ProjectInput{Name: "", FinanceMethod: "save"}, true},
		{"loan without term", model.ProjectInput{Name: "Solar", FinanceMethod: "loan", LoanTermMonths: 0}, true},
		{"loan negative term", model.ProjectInput{Name: "Solar", FinanceMethod: "loan", LoanTermMonths: -3}, true},
		{"save ignores missing term", model.ProjectInput{Name: "Kitchen", FinanceMethod: "save", LoanTermMonths: 0}, false},
		{"valid loan start date", model.ProjectInput{Name: "Solar", FinanceMethod: "loan", LoanTermMonths: 60, LoanStartDate: "2026-09-01"}, false},
		{"bad loan start date", model.ProjectInput{Name: "Solar", FinanceMethod: "loan", LoanTermMonths: 60, LoanStartDate: "next march"}, true},
		{"impossible loan start date", model.ProjectInput{Name: "Solar", FinanceMethod: "loan", LoanTermMonths: 60, LoanStartDate: "2026-02-31"}, true},
		{"empty start date ok", model.ProjectInput{Name: "Solar", FinanceMethod: "loan", LoanTermMonths: 60, LoanStartDate: ""}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := validateProject(tc.in)
			if (got != "") != tc.wantErr {
				t.Errorf("validateProject(%+v) = %q, wantErr=%v", tc.in, got, tc.wantErr)
			}
		})
	}
}

func TestStillToDraw(t *testing.T) {
	plan := model.ProjectPlan{Financed: 1500}
	loan := &model.Project{FinanceMethod: "loan", LoanTermMonths: 48, DepositPaid: 1500}
	if got := stillToDraw(loan, plan); got != 0 {
		t.Errorf("paid loan deposit: stillToDraw = %v want 0", got)
	}
	partial := &model.Project{FinanceMethod: "loan", LoanTermMonths: 48, DepositPaid: 1000}
	if got := stillToDraw(partial, plan); got != 500 {
		t.Errorf("partial deposit: stillToDraw = %v want 500", got)
	}
	// Switching the method back to "save" must not double-count the paid cash.
	switched := &model.Project{FinanceMethod: "save", DepositPaid: 1500}
	if got := stillToDraw(switched, model.ProjectPlan{Financed: 2000}); got != 500 {
		t.Errorf("method switched after paying: stillToDraw = %v want 500", got)
	}
	// Overpaid clamps at zero rather than going negative.
	over := &model.Project{FinanceMethod: "loan", LoanTermMonths: 48, DepositPaid: 9999}
	if got := stillToDraw(over, plan); got != 0 {
		t.Errorf("overpaid: stillToDraw = %v want 0", got)
	}
	// No deposit paid: full Financed still to draw.
	fresh := &model.Project{FinanceMethod: "save"}
	if got := stillToDraw(fresh, plan); got != 1500 {
		t.Errorf("fresh: stillToDraw = %v want 1500", got)
	}
}
