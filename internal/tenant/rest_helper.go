package tenant

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"github.com/sourcegraph/zoekt/internal/tenant/internal/tenanttype"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func InjectTenantFromHeader(ctx context.Context, header http.Header) (context.Context, error) {
	log.Printf("header: %v", header)
	tenantID := header.Get("X-TENANT-ID") // TODO: we don't use headerKeyTenantID here so we don't need to change it and potentially break future grpc changes
	log.Printf("tenantID: %s", tenantID)
	if tenantID != "" {
		tenant, err := tenanttype.Unmarshal(tenantID)
		if err != nil {
			return ctx, status.New(codes.InvalidArgument, fmt.Errorf("bad tenant value in header: %w", err).Error()).Err()
		}

		return tenanttype.WithTenant(ctx, tenant), nil
	}
	return ctx, nil
}
