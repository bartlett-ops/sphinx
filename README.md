# Sphinx

## Notes

### Per request
* Get username header (if exists)
* Get client IP
* Write to configmap
* trigger sync

### Sync (run periodically in background)
* Read configmap
* Convert values to list
* Write values list to middleware

