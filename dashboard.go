package main

import (
	"html/template"
	"net/http"
	"sort"
	"strings"
	"time"
)

type dashboardRow struct {
	Model     string
	App       string
	Requests  uint64
	TokensIn  uint64
	TokensOut uint64
	CostUSD   float64
	AvgLatMs  float64
	Errors    uint64
}

type dashboardData struct {
	Rows           []dashboardRow
	Recent         []requestRecord
	TotalRequests  uint64
	TotalTokensIn  uint64
	TotalTokensOut uint64
	TotalCostUSD   float64
	Uptime         string
}

// dashboardData builds a snapshot of the metrics for rendering. Called with the
// metrics mutex already held.
func (m *Metrics) dashboardData() dashboardData {
	maSet := map[string]bool{}
	for k := range m.tokensIn {
		maSet[k] = true
	}
	for k := range m.latency {
		maSet[k] = true
	}

	d := dashboardData{Rows: make([]dashboardRow, 0, len(maSet))}
	for k := range maSet {
		p := strings.SplitN(k, "|", 2)
		model, app := p[0], p[1]

		var reqs, errs uint64
		for rk, v := range m.requests {
			if strings.HasPrefix(rk, k+"|") {
				reqs += v
			}
		}
		for ek, v := range m.errors {
			if strings.HasPrefix(ek, k+"|") {
				errs += v
			}
		}
		var avgMs float64
		if h := m.latency[k]; h != nil && h.count > 0 {
			avgMs = h.sum / float64(h.count) * 1000
		}

		d.Rows = append(d.Rows, dashboardRow{
			Model:     model,
			App:       app,
			Requests:  reqs,
			TokensIn:  m.tokensIn[k],
			TokensOut: m.tokensOut[k],
			CostUSD:   m.costUSD[k],
			Errors:    errs,
			AvgLatMs:  avgMs,
		})
		d.TotalRequests += reqs
		d.TotalTokensIn += m.tokensIn[k]
		d.TotalTokensOut += m.tokensOut[k]
		d.TotalCostUSD += m.costUSD[k]
	}
	sort.Slice(d.Rows, func(i, j int) bool { return d.Rows[i].CostUSD > d.Rows[j].CostUSD })

	d.Recent = make([]requestRecord, len(m.recent))
	for i := range m.recent {
		d.Recent[len(m.recent)-1-i] = m.recent[i]
	}
	d.Uptime = time.Since(m.start).Round(time.Second).String()
	return d
}

// Dashboard serves the live HTML metrics dashboard.
func (m *Metrics) Dashboard() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = dashboardTmpl.Execute(w, m.dashboardData())
	})
}

var dashboardTmpl = template.Must(template.New("dashboard").Parse(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta http-equiv="refresh" content="5">
<title>Bifrost</title>
<style>
body{background:#0a0e1a;color:#c8d6e5;font-family:system-ui,-apple-system,sans-serif;margin:2rem auto;max-width:1000px;padding:0 1rem}
h1{color:#7fd4ff;font-weight:600}
h2{color:#9fb8cc;font-size:1rem;margin-top:2rem;text-transform:uppercase;letter-spacing:.05em}
.sub{color:#6b7a8a;font-weight:400;font-size:1rem;margin-left:.5rem}
.totals{display:flex;gap:1rem;flex-wrap:wrap;margin:1.5rem 0}
.total{border:1px solid #1e2a3a;border-radius:8px;padding:1rem 1.4rem;min-width:120px}
.total .v{font-size:1.6rem;color:#7fd4ff;font-family:ui-monospace,monospace}
.total .l{color:#6b7a8a;font-size:.78rem;margin-top:.2rem}
table{border-collapse:collapse;width:100%;margin:.75rem 0 1.5rem;font-size:.9rem}
th,td{border-bottom:1px solid #1e2a3a;padding:.45rem .6rem;text-align:right}
th:first-child,td:first-child{text-align:left}
th{color:#7fd4ff;font-weight:600}
.mono{font-family:ui-monospace,monospace}
.ok{color:#3ddc84}
.err{color:#ff6b6b}
.foot{color:#6b7a8a;font-size:.78rem}
a{color:#7fd4ff}
</style>
</head>
<body>
<h1>Bifrost<span class="sub">LLM gateway</span></h1>
<div class="totals">
  <div class="total"><div class="v">${{printf "%.6f" .TotalCostUSD}}</div><div class="l">total cost (USD)</div></div>
  <div class="total"><div class="v">{{.TotalRequests}}</div><div class="l">requests</div></div>
  <div class="total"><div class="v">{{.TotalTokensIn}}</div><div class="l">tokens in</div></div>
  <div class="total"><div class="v">{{.TotalTokensOut}}</div><div class="l">tokens out</div></div>
  <div class="total"><div class="v">{{.Uptime}}</div><div class="l">uptime</div></div>
</div>

<h2>By model / app</h2>
<table>
<tr><th>model</th><th>app</th><th>requests</th><th>tokens in</th><th>tokens out</th><th>cost</th><th>avg latency</th><th>errors</th></tr>
{{range .Rows}}<tr>
<td class="mono">{{.Model}}</td><td>{{.App}}</td><td>{{.Requests}}</td><td>{{.TokensIn}}</td><td>{{.TokensOut}}</td>
<td class="mono">${{printf "%.6f" .CostUSD}}</td><td class="mono">{{printf "%.0f" .AvgLatMs}} ms</td>
<td class="{{if .Errors}}err{{end}}">{{.Errors}}</td>
</tr>{{end}}
</table>

<h2>Recent requests</h2>
<table>
<tr><th>time (UTC)</th><th>app</th><th>model</th><th>endpoint</th><th>status</th><th>tok in</th><th>tok out</th><th>cost</th><th>latency</th></tr>
{{range .Recent}}<tr>
<td class="mono">{{.Time.Format "15:04:05"}}</td><td>{{.App}}</td><td class="mono">{{.Model}}</td><td class="mono">{{.Endpoint}}</td>
<td class="{{if .Err}}err{{else}}ok{{end}}">{{.Status}}</td>
<td>{{.TokensIn}}</td><td>{{.TokensOut}}</td><td class="mono">${{printf "%.6f" .CostUSD}}</td><td class="mono">{{printf "%.0f" .DurationMs}} ms</td>
</tr>{{end}}
</table>

<p class="foot">Prometheus format at <a href="/metrics">/metrics</a> · auto-refreshes every 5s</p>
</body>
</html>
`))
