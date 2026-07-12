package fixture

import fs "os"

func newHelper() { _, _ = fs.ReadFile("rejected") }
