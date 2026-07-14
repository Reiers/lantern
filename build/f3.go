// F3 network manifests embedded into the binary. The JSON files are copied
// verbatim from lotus/build/buildconstants/f3manifest_*.json at commit
// a0ecb8687f1c60d5e66040b6de364dbc9cc4d253. See LICENSE.

package build

import _ "embed"

//go:embed f3manifest_mainnet.json
var F3ManifestMainnetJSON []byte

//go:embed f3manifest_calibnet.json
var F3ManifestCalibnetJSON []byte
