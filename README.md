# Sphinx

Authentication Service

## Notes

### ForwardAuth
Configure [ForwardAuth](https://doc.traefik.io/traefik/reference/routing-configuration/http/middlewares/forwardauth/) to forward request to sphinx

### Sphinx

### Per request
* Get username header (if exists)
* Get client IP
* Write to configmap
* trigger sync

### Sync (run periodically in background)
* Read configmap
* Convert values to list
* Write values list to middleware

