package upstream

// recordModelCatalog keeps realm membership separate from account cooldowns and
// catalog TTLs. Failed/empty fetches must not erase the last successful catalog.
func (c *Client) recordModelCatalog(realm string, infos []ModelInfo) {
	if (realm != "cn" && realm != "global") || len(infos) == 0 {
		return
	}
	ids := make(map[string]bool, len(infos))
	for _, mi := range infos {
		if mi.ID != "" {
			ids[mi.ID] = true
		}
	}
	if len(ids) == 0 {
		return
	}
	c.modelCatalogMu.Lock()
	if c.modelCatalog == nil {
		c.modelCatalog = make(map[string]map[string]bool)
	}
	c.modelCatalog[realm] = ids
	c.modelCatalogMu.Unlock()
}

// ModelRealms reports positive membership in successful catalog snapshots.
// Both false means unknown, not a claim that the model is unavailable globally.
// It never performs network I/O or expires when an account's cooldown ends.
func (c *Client) ModelRealms(model string) (cn, global bool) {
	c.modelCatalogMu.RLock()
	defer c.modelCatalogMu.RUnlock()
	return c.modelCatalog["cn"][model], c.modelCatalog["global"][model]
}

// ExclusiveModelRealm returns the catalog group for a model listed in one realm.
func (c *Client) ExclusiveModelRealm(model string) string {
	cn, global := c.ModelRealms(model)
	if cn && !global {
		return "cn"
	}
	if global && !cn {
		return "global"
	}
	return ""
}
