# trixie: LibreOffice 25.x. В LibreOffice 7.4 (bookworm) при экспорте в PDF теряются текстовые гиперссылки
FROM golang:1.24-trixie

# Disable prompts on apt-get install
ENV DEBIAN_FRONTEND noninteractive

WORKDIR /app

RUN apt-get update -qq && \
	apt-get -y -q install \
		libreoffice \
		fonts-opensymbol \
		hyphen-fr \
		hyphen-de \
		hyphen-en-us \
		hyphen-it \
		hyphen-ru \
		fonts-dejavu \
		fonts-dejavu-core \
		fonts-dejavu-extra \
		fonts-droid-fallback \
		fonts-dustin \
		fonts-f500 \
		fonts-fanwood \
		fonts-freefont-ttf \
		fonts-liberation \
		fonts-lmodern \
		fonts-lyx \
		fonts-sil-gentium \
		fonts-texgyre \
		fonts-tlwg-purisa \
        ffmpeg \
        imagemagick \
	&& apt-get -y -q remove libreoffice-gnome && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/*


COPY ./dockerize/imagemagic/policy.xml /tmp/policy.xml

# путь к политике зависит от версии ImageMagick (6 или 7)
RUN for dir in /etc/ImageMagick-*; do cp /tmp/policy.xml "$dir/policy.xml"; done && rm /tmp/policy.xml

COPY go.mod .

COPY go.sum .

RUN go mod download

COPY . .

RUN go build -o consumer ./cmd/consumer

CMD ["./consumer"]