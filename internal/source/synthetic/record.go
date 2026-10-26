package synthetic

// Record is one generated log's field set; host and app form the stream.
type Record struct {
	Host     *HostInfo
	App      *AppInfo
	Level    string
	Priority int // syslog priority, 0-191
	Message  string

	// per-line values of the app's enum and dynamic fields;
	// LineVals[i] pairs with App.Lines[i]
	LineVals []string
}
