## Generate File in Version 2.4.0
```
cd docs/asyncapi-gen
go generate ./...
```

## Convert to Version 3.0.0
```
npx --yes @asyncapi/cli@6.2.0 convert docs/asyncapi-gen/asyncapi.json -f asyncapi -t 3.0.0 -o docs/asyncapi.json
```

alternatively, copy/paste `docs/asyncapi-gen/asyncapi.json` into https://studio.asyncapi.com and click 'Convert Document'
