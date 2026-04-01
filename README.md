# Sphinx

Authentication Service

## Notes

### ForwardAuth
Configure [ForwardAuth](https://doc.traefik.io/traefik/reference/routing-configuration/http/middlewares/forwardauth/) to forward request to sphinx

### Sphinx

### Per request
* Get username header (if exists)
* Get client IP
* Check if in cache
* Add to cache
* trigger sync

### Sync (run periodically in background)
* Write cache to Middleware
