Third-party licenses for safe_sql
=================================

safe_sql itself is MIT licensed (see LICENSE). It links the following Go
modules; each is used unmodified under its own license.

{{ range . }}
{{ .Name }}
  License: {{ .LicenseName }}
  Source:  {{ .LicenseURL }}
{{ end }}
