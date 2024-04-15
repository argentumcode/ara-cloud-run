# ara(ARA records ansible) authentication proxy for Cloud Run

This is a wrapper program designed to access [ara](https://ara.recordsansible.org) deployed on Cloud Run using service account authentication.

## Installation

Download from Release page.

## Usage

ADC(with Service account)

```
$ ara-cloud-run --cloud-run-url=https://ara-example.run.app -- ansible-playbook playbook.yml
```

With service account impersonatation

```
$ ara-cloud-run --cloud-run-url=https://ara-example.run.app --impersonate-service-account=service@example.iam.gserviceaccount.com -- ansible-playbook playbook.yml
```
