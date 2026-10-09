# Fixture repository whose port adapters lie inside a declared module

## Test units

This repository declares its test units in `config/test-units.json`. Tests observe a declared module only through its entry, except that a port's contract suite, its contract runs and an adapter's own tests may import that port's adapters inside the module; a declared port is observed only through the port and its in-memory adapter; and the repository's dependency checker enforces this for production code and tests alike, with the same exemption. A test that imports a file behind an entry states why in a line `test-boundary-exception: <reason>`. The rules are in the `workflow/test-quality` standard, section "Declared test units".
