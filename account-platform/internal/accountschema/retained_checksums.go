package accountschema

// Exact retained originals; business SQL bytes are never rewritten by hooks.
var bridgeChecksums = map[int64]string{
	27: "cfd14d187cd1c3fa3aab134de59cbfb88a44a7bbc88bc3e3406d8d6c7d2505db",
	28: "05eec7870758d9eb29248236a389f7facfbc4025d1fa711d40775d3fe96855b9",
	33: "7c7e8d6e507d22b28e63ffabec95fd3ae375f9f686207d17af3b7bd24b624bf6",
}
var retainedScopeChecksums = []string{
	"658324df04b4e19fb98531c192f409fb68b82f5d0f027fe6b65527ebddca8637",
	"7e4ea64626e271cb51b138b945812380e2b0ee4c2ab58300408e67baf66ab5d0",
}
