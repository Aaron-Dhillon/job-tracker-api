package applications_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aaron-Dhillon/job-tracker-api/internal/applications"
	"github.com/Aaron-Dhillon/job-tracker-api/internal/workflow"
)

func TestDate_RoundTrip(t *testing.T) {
	var d applications.Date
	require.NoError(t, json.Unmarshal([]byte(`"2026-09-11"`), &d))

	assert.Equal(t, 2026, d.Time().Year())
	assert.Equal(t, time.September, d.Time().Month())
	assert.Equal(t, 11, d.Time().Day())

	encoded, err := json.Marshal(d)
	require.NoError(t, err)
	assert.JSONEq(t, `"2026-09-11"`, string(encoded))
}

// The column is a `date`. Accepting a timestamp would silently drop its time
// and zone, so a client sending 23:00Z would see the date shift under it.
func TestDate_RejectsAnythingButYYYYMMDD(t *testing.T) {
	for _, raw := range []string{
		`"2026-09-11T00:00:00Z"`,
		`"11/09/2026"`,
		`"2026-13-01"`,
		`"not a date"`,
		`20260911`,
		`null`,
		`""`,
	} {
		t.Run(raw, func(t *testing.T) {
			var d applications.Date
			assert.Error(t, json.Unmarshal([]byte(raw), &d))
		})
	}
}

// The distinction a plain *string cannot express: absent means "leave it
// alone", explicit null means "clear it".
func TestOptional_AbsentVersusNull(t *testing.T) {
	var absent applications.PatchInput
	require.NoError(t, json.Unmarshal([]byte(`{"role_title":"SRE"}`), &absent))

	assert.True(t, absent.RoleTitle.Set)
	require.NotNil(t, absent.RoleTitle.Value)
	assert.Equal(t, "SRE", *absent.RoleTitle.Value)
	assert.False(t, absent.Notes.Set, "an absent key must not be Set")
	assert.Nil(t, absent.Notes.Value)

	var cleared applications.PatchInput
	require.NoError(t, json.Unmarshal([]byte(`{"notes":null}`), &cleared))

	assert.True(t, cleared.Notes.Set, "an explicit null is a request to clear")
	assert.Nil(t, cleared.Notes.Value)
}

func TestPatchInput_Validate(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "updates role_title", body: `{"role_title":"SRE"}`},
		{name: "clears notes", body: `{"notes":null}`},
		{name: "clears location", body: `{"location":null}`},
		{name: "updates applied_on", body: `{"applied_on":"2026-01-02"}`},
		{
			name:    "status is rejected by name",
			body:    `{"status":"offer"}`,
			wantErr: "use POST /applications/{id}/transition",
		},
		{
			name:    "status rejected even alongside a real field",
			body:    `{"role_title":"SRE","status":"offer"}`,
			wantErr: "use POST /applications/{id}/transition",
		},
		{name: "empty body changes nothing", body: `{}`, wantErr: "no fields to update"},
		{name: "role_title cannot be null", body: `{"role_title":null}`, wantErr: "cannot be null"},
		{name: "role_title cannot be blank", body: `{"role_title":"   "}`, wantErr: "cannot be empty"},
		{name: "applied_on cannot be null", body: `{"applied_on":null}`, wantErr: "cannot be null"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var in applications.PatchInput
			require.NoError(t, json.Unmarshal([]byte(tt.body), &in))

			err := in.Validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// status is a declared field rather than an unknown one so the 400 can name
// the transition endpoint. A generic "unknown field status" would be correct
// and useless.
func TestPatchInput_StatusErrorNamesTheEndpoint(t *testing.T) {
	var in applications.PatchInput
	require.NoError(t, json.Unmarshal([]byte(`{"status":"offer"}`), &in))

	err := in.Validate()
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "unknown field")
	assert.Contains(t, err.Error(), "transition")
}

func TestPatchInput_TrimsRoleTitle(t *testing.T) {
	var in applications.PatchInput
	require.NoError(t, json.Unmarshal([]byte(`{"role_title":"  Staff Engineer  "}`), &in))
	require.NoError(t, in.Validate())

	require.NotNil(t, in.RoleTitle.Value)
	assert.Equal(t, "Staff Engineer", *in.RoleTitle.Value)
}

func TestCreateInput_Validate(t *testing.T) {
	tests := []struct {
		name    string
		in      applications.CreateInput
		wantErr string
	}{
		{name: "valid", in: applications.CreateInput{CompanyName: "Visa", RoleTitle: "SRE"}},
		{name: "trims", in: applications.CreateInput{CompanyName: "  Visa  ", RoleTitle: "  SRE  "}},
		{name: "company required", in: applications.CreateInput{RoleTitle: "SRE"}, wantErr: "company_name is required"},
		{name: "blank company", in: applications.CreateInput{CompanyName: "   ", RoleTitle: "SRE"}, wantErr: "company_name is required"},
		{name: "role required", in: applications.CreateInput{CompanyName: "Visa"}, wantErr: "role_title is required"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := tt.in
			err := in.Validate()
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "Visa", in.CompanyName)
			assert.Equal(t, "SRE", in.RoleTitle)
		})
	}
}

// The 409 body is a contract phase 2 pinned at the workflow level; this is the
// HTTP shape that carries it.
func TestInvalidTransitionBody_MarshalsAllowedAsArray(t *testing.T) {
	terminal := applications.InvalidTransitionBody{
		Error:   "invalid transition",
		From:    "rejected",
		Allowed: []workflow.State{},
	}
	encoded, err := json.Marshal(terminal)
	require.NoError(t, err)
	assert.JSONEq(t, `{"error":"invalid transition","from":"rejected","allowed":[]}`, string(encoded))
	assert.NotContains(t, string(encoded), "null")
}
