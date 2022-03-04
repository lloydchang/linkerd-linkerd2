use crate::api::policy::{AuthorizationPolicy, AuthorizationPolicySpec, Server, ServerSpec};
use anyhow::{anyhow, bail, Result};
use futures::future;
use hyper::{body::Buf, http, Body, Request, Response};
use k8s_openapi::serde::de::DeserializeOwned;
use kube::{
    core::{DynamicObject, GroupVersionKind},
    Resource, ResourceExt,
};
use linkerd_policy_controller_k8s_index::{Index, SharedIndex};
use std::task;
use thiserror::Error;
use tracing::{debug, info, warn};

#[derive(Clone)]
pub struct AdmissionService {
    pub index: SharedIndex,
}

#[derive(Debug, Error)]
pub enum Error {
    #[error("failed to read request body: {0}")]
    Request(#[from] hyper::Error),

    #[error("failed to encode json response: {0}")]
    Json(#[from] serde_json::Error),
}

type Review = kube::core::admission::AdmissionReview<DynamicObject>;
type AdmissionRequest = kube::core::admission::AdmissionRequest<DynamicObject>;
type AdmissionResponse = kube::core::admission::AdmissionResponse;
type AdmissionReview = kube::core::admission::AdmissionReview<DynamicObject>;

// === impl AdmissionService ===

impl hyper::service::Service<Request<Body>> for AdmissionService {
    type Response = Response<Body>;
    type Error = Error;
    type Future = future::BoxFuture<'static, Result<Response<Body>, Error>>;

    fn poll_ready(&mut self, _cx: &mut task::Context<'_>) -> task::Poll<Result<(), Error>> {
        task::Poll::Ready(Ok(()))
    }

    fn call(&mut self, req: Request<Body>) -> Self::Future {
        if req.method() != http::Method::POST || req.uri().path() != "/" {
            return Box::pin(future::ok(
                Response::builder()
                    .status(http::StatusCode::NOT_FOUND)
                    .body(Body::empty())
                    .expect("not found response must be valid"),
            ));
        }

        let index = self.index.clone();
        Box::pin(async move {
            let bytes = hyper::body::aggregate(req.into_body()).await?;
            let review: Review = match serde_json::from_reader(bytes.reader()) {
                Ok(review) => review,
                Err(error) => {
                    warn!(%error, "failed to parse request body");
                    return json_response(AdmissionResponse::invalid(error).into_review());
                }
            };

            let rsp = match review.try_into() {
                Ok(req) => {
                    debug!(?req);
                    admit(req, &index)
                }
                Err(error) => {
                    warn!(%error, "invalid admission request");
                    AdmissionResponse::invalid(error)
                }
            };

            // If validation fails, deny admission.
            debug!(?rsp);
            json_response(rsp.into_review())
        })
    }
}

fn admit(req: AdmissionRequest, index: &SharedIndex) -> AdmissionResponse {
    let GroupVersionKind {
        group,
        version,
        kind,
    } = &req.kind;

    if *group == *AuthorizationPolicy::group(&()) && *kind == *AuthorizationPolicy::kind(&()) {
        return admit_authz_policy(req);
    }

    if *group == *Server::group(&()) && kind == &*Server::kind(&()) {
        return admit_server(req, index);
    }

    warn!(%group, %version, %kind, "unsupported resource type");
    AdmissionResponse::invalid(format_args!(
        "unsupported resource type: {}.{}.{}",
        group, version, kind
    ))
}

// === AuthorizationPolicy ===

fn admit_authz_policy(req: AdmissionRequest) -> AdmissionResponse {
    let rsp = AdmissionResponse::from(&req);
    let (ns, name, spec) = match parse_spec(req) {
        Ok(s) => s,
        Err(error) => {
            warn!(%error, "failed to deserialize server from admission request");
            return AdmissionResponse::invalid(error);
        }
    };

    match validate_authz_policy(spec) {
        Ok(()) => rsp,
        Err(error) => {
            info!(%error, %ns, %name, "denying AuthorizationPolicy");
            rsp.deny(error)
        }
    }
}

fn validate_authz_policy(spec: AuthorizationPolicySpec) -> Result<()> {
    if spec.target_ref.group.as_deref() != Some(&*Server::group(&()))
        || *spec.target_ref.kind != *Server::kind(&())
    {
        bail!("targetRef must be a policy.linkerd.io Server");
    }

    Ok(())
}

// === Server ===

fn admit_server(req: AdmissionRequest, index: &SharedIndex) -> AdmissionResponse {
    let rsp = AdmissionResponse::from(&req);
    let (ns, name, spec) = match parse_spec(req) {
        Ok(s) => s,
        Err(error) => {
            warn!(%error, "failed to deserialize server from admission request");
            return AdmissionResponse::invalid(error);
        }
    };

    match validate_server(&ns, &name, spec, &*index.read()) {
        Ok(()) => rsp,
        Err(error) => {
            info!(%error, %ns, %name, "denying server");
            rsp.deny(error)
        }
    }
}

/// Validates a new server (`review`) against existing `servers`.
fn validate_server(ns: &str, name: &str, spec: ServerSpec, index: &Index) -> Result<()> {
    if let Some(nsidx) = index.get_ns(ns) {
        for (srvname, srv) in nsidx.servers.iter() {
            // If the port and pod selectors select the same resources, fail the admission of the
            // server. Ignore existing instances of this Server (e.g., if the server's metadata is
            // changing).
            if *srvname != name
                // TODO(ver) this isn't rigorous about detecting servers that select the same port if one port
                // specifies a numeric port and the other specifies the port's name.
                && *srv.port() == spec.port
                // TODO(ver) We can probably detect overlapping selectors more effectively.
                && *srv.pod_selector() == spec.pod_selector
            {
                bail!("identical server spec already exists");
            }
        }
    }

    Ok(())
}

// === utils ===

fn parse_spec<T: DeserializeOwned>(req: AdmissionRequest) -> Result<(String, String, T)> {
    let obj = req.object.ok_or_else(|| anyhow!("missing server"))?;
    let ns = obj
        .namespace()
        .ok_or_else(|| anyhow!("no 'namespace' field set on server"))?;
    let name = obj.name();
    let data = obj
        .data
        .get("spec")
        .cloned()
        .ok_or_else(|| anyhow!("no 'spec' field set on server"))?;
    let spec = serde_json::from_value(data)?;
    Ok((ns, name, spec))
}

fn json_response(rsp: AdmissionReview) -> Result<Response<Body>, Error> {
    let bytes = serde_json::to_vec(&rsp)?;
    Ok(Response::builder()
        .status(http::StatusCode::OK)
        .header(http::header::CONTENT_TYPE, "application/json")
        .body(Body::from(bytes))
        .expect("admission review response must be valid"))
}
