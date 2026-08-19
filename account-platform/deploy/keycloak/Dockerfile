FROM maven:3.9.11-eclipse-temurin-21 AS provider
WORKDIR /src
COPY mobile-authenticator/pom.xml ./pom.xml
COPY mobile-authenticator/src ./src
RUN mvn -q -DskipTests package

FROM quay.io/keycloak/keycloak:26.7.0 AS builder
COPY --from=provider /src/target/pinvou-mobile-authenticator-1.0.0.jar /opt/keycloak/providers/pinvou-mobile-authenticator.jar
RUN /opt/keycloak/bin/kc.sh build

FROM quay.io/keycloak/keycloak:26.7.0
COPY --from=builder /opt/keycloak/ /opt/keycloak/
