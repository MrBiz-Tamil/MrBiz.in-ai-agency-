// Typed ConnectRPC clients for the control plane, bound to the signed-in user's bearer token.
import { Code, ConnectError, createClient, type Client, type Interceptor } from '@connectrpc/connect';
import { createConnectTransport } from '@connectrpc/connect-web';
import { ApprovalService } from '$lib/gen/venture/approval/v1/approval_pb';
import { ConnectionService } from '$lib/gen/venture/connection/v1/connection_pb';
import { ControlsService } from '$lib/gen/venture/controls/v1/controls_pb';
import { CrmService } from '$lib/gen/venture/crm/v1/crm_pb';
import { GenesisService } from '$lib/gen/venture/genesis/v1/genesis_pb';
import { PortfolioService } from '$lib/gen/venture/portfolio/v1/portfolio_pb';
import { ProvisionerService } from '$lib/gen/venture/provisioner/v1/provisioner_pb';
import { TenantService } from '$lib/gen/venture/tenant/v1/tenant_pb';
import { config } from './config';

export interface Api {
	tenant: Client<typeof TenantService>;
	genesis: Client<typeof GenesisService>;
	provisioner: Client<typeof ProvisionerService>;
	approval: Client<typeof ApprovalService>;
	crm: Client<typeof CrmService>;
	connection: Client<typeof ConnectionService>;
	controls: Client<typeof ControlsService>;
	portfolio: Client<typeof PortfolioService>;
}

/** Clients that call the control plane as the user holding `accessToken`. */
export function api(accessToken: string): Api {
	const bearer: Interceptor = (next) => async (req) => {
		req.header.set('Authorization', `Bearer ${accessToken}`);
		return next(req);
	};
	const transport = createConnectTransport({
		baseUrl: config().controlplaneUrl,
		interceptors: [bearer],
		// Provisioning legitimately takes up to a minute; everything else should answer well within this.
		defaultTimeoutMs: 120_000
	});
	return {
		tenant: createClient(TenantService, transport),
		genesis: createClient(GenesisService, transport),
		provisioner: createClient(ProvisionerService, transport),
		approval: createClient(ApprovalService, transport),
		crm: createClient(CrmService, transport),
		connection: createClient(ConnectionService, transport),
		controls: createClient(ControlsService, transport),
		portfolio: createClient(PortfolioService, transport)
	};
}

export interface Failure {
	status: number;
	code: string;
	message: string;
}

const HTTP: Partial<Record<Code, number>> = {
	[Code.InvalidArgument]: 400,
	[Code.Unauthenticated]: 401,
	[Code.PermissionDenied]: 403,
	[Code.NotFound]: 404,
	[Code.AlreadyExists]: 409,
	[Code.Aborted]: 409,
	[Code.FailedPrecondition]: 412,
	[Code.ResourceExhausted]: 429,
	[Code.Unavailable]: 503,
	[Code.DeadlineExceeded]: 504
};

/** Converts any thrown value into something safe to show a user. Unexpected errors are not leaked. */
export function toFailure(e: unknown): Failure {
	if (e instanceof ConnectError) {
		const status = HTTP[e.code] ?? 500;
		const expected = status < 500 || e.code === Code.Unavailable;
		return {
			status,
			code: Code[e.code] ?? 'Unknown',
			message: expected ? e.rawMessage : 'The service had a problem. Nothing was changed. Please try again.'
		};
	}
	return { status: 500, code: 'Internal', message: 'Something went wrong. Please try again.' };
}
