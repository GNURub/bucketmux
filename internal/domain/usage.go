package domain

type ProviderBucketUsage struct {
	ProviderAccountID string
	Bucket            string
	ObjectCount       int64
	Bytes             int64
}
