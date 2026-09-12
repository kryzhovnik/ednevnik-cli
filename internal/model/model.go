package model

import "time"

const SchemaVersion = 2

type Student struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	School     string `json:"school,omitempty"`
	Class      string `json:"class,omitempty"`
	SchoolYear string `json:"school_year,omitempty"`
	Current    bool   `json:"current"`
	Selected   bool   `json:"selected,omitempty"`
}

type Subject struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Teacher         string   `json:"teacher,omitempty"`
	DisplayedGrades []string `json:"displayed_grades"`
}

type Grade struct {
	ID        string `json:"id"`
	StudentID string `json:"student_id"`
	SubjectID string `json:"subject_id"`
	Subject   string `json:"subject"`
	Value     string `json:"value"`
	Kind      string `json:"kind,omitempty"`
	Date      string `json:"date,omitempty"`
	Note      string `json:"note,omitempty"`
}

type Absence struct {
	ID        string `json:"id"`
	StudentID string `json:"student_id"`
	Subject   string `json:"subject,omitempty"`
	Date      string `json:"date,omitempty"`
	Period    string `json:"period,omitempty"`
	Status    string `json:"status,omitempty"`
	Note      string `json:"note,omitempty"`
}

// Activity is one item in the portal timeline. The portal uses the timeline for
// grades, teacher observations, absences, and other events.
type Activity struct {
	ID        string `json:"id"`
	StudentID string `json:"student_id"`
	PortalID  int64  `json:"portal_id"`
	Date      string `json:"date,omitempty"`
	Day       string `json:"day,omitempty"`
	Type      string `json:"type,omitempty"`
	TypeName  string `json:"type_name,omitempty"`
	Title     string `json:"title,omitempty"`
	Symbol    string `json:"symbol,omitempty"`
	Subtitle  string `json:"subtitle,omitempty"`
	Note      string `json:"note,omitempty"`
	URL       string `json:"url,omitempty"`
	IsNew     bool   `json:"is_new,omitempty"`
}

type ActivityPage struct {
	SchemaVersion int        `json:"schema_version"`
	CurrentPage   int        `json:"current_page"`
	NextPage      *int       `json:"next_page"`
	LastPage      int        `json:"last_page"`
	Items         []Activity `json:"items"`
}

type StudentData struct {
	Student    Student    `json:"student"`
	Subjects   []Subject  `json:"subjects"`
	Grades     []Grade    `json:"grades"`
	Absences   []Absence  `json:"absences"`
	Activities []Activity `json:"activities"`
}

type Snapshot struct {
	SchemaVersion int           `json:"schema_version"`
	FetchedAt     time.Time     `json:"fetched_at"`
	Students      []StudentData `json:"students"`
}

type Change struct {
	Kind      string `json:"kind"`
	StudentID string `json:"student_id"`
	RecordID  string `json:"record_id"`
	Summary   string `json:"summary"`
}

type Changes struct {
	SchemaVersion int       `json:"schema_version"`
	ComparedAt    time.Time `json:"compared_at"`
	Items         []Change  `json:"items"`
}
