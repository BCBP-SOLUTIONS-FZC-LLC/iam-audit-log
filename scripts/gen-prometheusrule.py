#!/usr/bin/env python3
"""Generate deploy/helm/templates/prometheusrule.yaml from the static rule files.

The static files under deploy/monitoring/ are the source of truth
(app-alerts.yml, recording-rules.yml, slo-rules.yml). This script
concatenates their groups into one PrometheusRule, templating the release
name into `job` selectors and escaping Prometheus template braces for Helm.
TestAlerts_StaticMirrorMatchesHelm fails if the two drift, so re-run this
after editing any rule file:

    python3 scripts/gen-prometheusrule.py
"""
import pathlib
import re

ROOT = pathlib.Path(__file__).resolve().parent.parent
SOURCES = ["app-alerts.yml", "recording-rules.yml", "slo-rules.yml"]
FULLNAME = '{{ include "iam-audit-log.fullname" . }}'

HEADER = '''{{- /*
PrometheusRule for iam-audit-log: the alerts, recording rules and SLOs (LLD
§11; Enterprise Platform Observability Standard, gap 46). GENERATED from
deploy/monitoring/{app-alerts,recording-rules,slo-rules}.yml by
scripts/gen-prometheusrule.py; do not edit by hand.
TestAlerts_StaticMirrorMatchesHelm keeps it identical to those files.
Prometheus template braces are escaped for Helm.
*/ -}}
{{- if .Values.prometheusRule.enabled }}
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: {{ include "iam-audit-log.fullname" . }}
  labels:
    {{- include "iam-audit-log.labels" . | nindent 4 }}
    {{- with .Values.prometheusRule.additionalLabels }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
spec:
  groups:
'''


def groups_body(text: str) -> str:
    i = text.index("\ngroups:\n")
    return text[i + len("\ngroups:\n"):].rstrip("\n")


def templatize(body: str) -> str:
    # Escape Prometheus {{ ... }} first, so Helm leaves them literal.
    body = re.sub(r"\{\{(.*?)\}\}", lambda m: "{{`{{" + m.group(1) + "}}`}}", body)
    body = body.replace('job="iam-audit-log"', 'job={{ include "iam-audit-log.fullname" . | quote }}')
    body = body.replace('job_name=~"iam-audit-log-', 'job_name=~"' + FULLNAME + '-')
    return body


def main() -> None:
    parts = []
    for name in SOURCES:
        text = (ROOT / "deploy" / "monitoring" / name).read_text()
        parts.append(templatize(groups_body(text)))
    body = "\n\n".join(parts)
    indented = "\n".join(("  " + line) if line else line for line in body.split("\n"))
    out = HEADER + indented + "\n{{- end }}\n"
    (ROOT / "deploy" / "helm" / "templates" / "prometheusrule.yaml").write_text(out)


if __name__ == "__main__":
    main()
