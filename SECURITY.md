# Security policy

## Reporting a vulnerability

Please report vulnerabilities **privately** through GitHub's
[private vulnerability reporting](https://github.com/accquix/register/security/advisories/new)
for this repository. Do not open a public issue.

Include what you found, how to reproduce it, and its impact. We aim to acknowledge a report within
5 working days and to agree a disclosure date with you.

## Scope

In scope: this repository's code, the container image built from it, and the default deployment in
`deploy/compose`. Of particular interest:

- authentication and authorisation of the API (keys, anonymous mode, the natural-person DIČ
  lookup);
- anything that exposes personal data beyond what the API is meant to return, or puts queries or
  identifiers into logs;
- injection through source files (the RPO bucket, the Financial Administration exports) or
  through API input;
- denial of service that bypasses the rate and fuzzy limits.

Out of scope: the official data sources themselves, and instances run by third parties (report to
their operators).

## Supported versions

Security fixes go to the latest release.

## No telemetry

The software sends nothing anywhere except requests to the official data sources.
