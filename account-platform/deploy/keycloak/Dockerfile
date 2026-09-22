FROM maven:3.9.11-eclipse-temurin-21 AS provider
WORKDIR /src
COPY mobile-authenticator/pom.xml ./pom.xml
COPY mobile-authenticator/src ./src
RUN mvn -q -DskipTests package

FROM quay.io/keycloak/keycloak:26.7.0@sha256:0f198be292568439d700cdbfb893e69a6009bb43a94a06a945b1d3d506c76b13 AS builder
ENV KC_DB=postgres KC_HEALTH_ENABLED=true KC_METRICS_ENABLED=true KC_FEATURES=token-exchange-standard:v2
COPY --from=provider --chown=keycloak:keycloak --chmod=644 /src/target/pinvou-mobile-authenticator-1.0.0.jar /opt/keycloak/providers/pinvou-mobile-authenticator.jar
COPY themes/pinvou /opt/keycloak/themes/pinvou
# A stable timestamp avoids a false provider-change error at start --optimized.
RUN touch -m --date=@1756684800 /opt/keycloak/providers/pinvou-mobile-authenticator.jar
RUN /opt/keycloak/bin/kc.sh build

FROM quay.io/keycloak/keycloak:26.7.0@sha256:0f198be292568439d700cdbfb893e69a6009bb43a94a06a945b1d3d506c76b13
COPY --from=builder /opt/keycloak/ /opt/keycloak/
ENV KC_DB=postgres KC_HEALTH_ENABLED=true KC_METRICS_ENABLED=true KC_FEATURES=token-exchange-standard:v2
