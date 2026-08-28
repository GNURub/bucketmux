package admin

import "strings"

type providerCatalogPreset struct {
	Key         string
	Brand       string
	Name        string
	Description string
}

var providerCatalog = []providerCatalogPreset{
	{Key: "aws", Brand: "aws", Name: "Amazon S3", Description: "AWS regions and S3 credentials"},
	{Key: "cloudflare", Brand: "cloudflare", Name: "Cloudflare R2", Description: "S3-compatible, zero egress fees"},
	{Key: "gcs", Brand: "gcs", Name: "Google Cloud Storage", Description: "XML API interoperability keys"},
	{Key: "backblaze", Brand: "backblaze", Name: "Backblaze B2", Description: "S3-compatible application keys"},
	{Key: "idrive", Brand: "idrive", Name: "IDrive e2", Description: "Region-specific S3-compatible storage"},
	{Key: "azure", Brand: "azure", Name: "Microsoft Azure Blob Storage", Description: "Native Azure Blob shared-key adapter"},
	{Key: "oci", Brand: "oci", Name: "Oracle Cloud Infrastructure (OCI) Object Storage", Description: "OCI S3 Compatibility API"},
	{Key: "digitalocean", Brand: "digitalocean", Name: "DigitalOcean Spaces", Description: "Region-aware Spaces endpoint"},
	{Key: "hetzner", Brand: "hetzner", Name: "Hetzner Object Storage", Description: "S3-compatible European locations"},
	{Key: "scaleway", Brand: "scaleway", Name: "Scaleway Object Storage", Description: "Regional S3-compatible endpoints"},
	{Key: "ovh", Brand: "ovh", Name: "OVHcloud Object Storage", Description: "S3-compatible public cloud storage"},
	{Key: "akamai", Brand: "akamai", Name: "Akamai Connected Cloud", Description: "Linode Object Storage S3 endpoints"},
	{Key: "wasabi", Brand: "wasabi", Name: "Wasabi", Description: "S3-compatible hot cloud storage"},
	{Key: "minio", Brand: "minio", Name: "MinIO", Description: "Self-hosted S3-compatible storage"},
	{Key: "cloudinary", Brand: "cloudinary", Name: "Cloudinary", Description: "Image storage and delivery API"},
	{Key: "vercel", Brand: "vercel", Name: "Vercel Blob", Description: "Read-write token configuration"},
	{Key: "local", Brand: "local", Name: "Local disk", Description: "Filesystem storage for this instance"},
	{Key: "custom", Brand: "custom", Name: "Custom S3-compatible", Description: "Any path-style S3 endpoint"},
}

func filterProviderCatalog(query string) []providerCatalogPreset {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return providerCatalog
	}
	out := make([]providerCatalogPreset, 0, len(providerCatalog))
	for _, preset := range providerCatalog {
		if strings.Contains(strings.ToLower(preset.Name+" "+preset.Description), query) {
			out = append(out, preset)
		}
	}
	return out
}
