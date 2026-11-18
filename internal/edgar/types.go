// Package edgar decodes SEC EDGAR's public submissions and ticker-lookup
// APIs. Like internal/rh, it only gives the raw data Go types; nothing here
// interprets a filing, and nothing here places a trade or contacts a
// broker — it reads public filings only.
package edgar

// CompanyTicker is one entry from
// https://www.sec.gov/files/company_tickers.json.
type CompanyTicker struct {
	CIK    int    `json:"cik_str"`
	Ticker string `json:"ticker"`
	Title  string `json:"title"`
}

// submissionsRaw mirrors https://data.sec.gov/submissions/CIK##########.json.
// Filings arrive as parallel ("columnar") arrays; zipSubmissions turns them
// into one Filing per row.
type submissionsRaw struct {
	CIK     string   `json:"cik"`
	Name    string   `json:"name"`
	Tickers []string `json:"tickers"`
	Filings struct {
		Recent filingColumns `json:"recent"`
	} `json:"filings"`
}

type filingColumns struct {
	AccessionNumber []string `json:"accessionNumber"`
	FilingDate      []string `json:"filingDate"`
	ReportDate      []string `json:"reportDate"`
	Form            []string `json:"form"`
	Items           []string `json:"items"`
	PrimaryDocument []string `json:"primaryDocument"`
}

// Submissions is a company's filing history, decoded from the columnar raw
// response into one Filing per row, newest first (as SEC returns it).
type Submissions struct {
	CIK     string
	Name    string
	Tickers []string
	Filings []Filing
}

// Filing is one row of a company's filing history.
type Filing struct {
	AccessionNumber string
	FilingDate      string // YYYY-MM-DD
	ReportDate      string // YYYY-MM-DD; empty for forms with no report period
	Form            string // "10-K", "10-Q", "8-K", "4", ...
	Items           string // 8-K item numbers, comma-separated; empty otherwise
	PrimaryDocument string
}
