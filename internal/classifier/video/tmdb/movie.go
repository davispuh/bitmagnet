package tmdb

import (
	"context"
	"errors"
	"fmt"
	"github.com/bitmagnet-io/bitmagnet/internal/classifier"
	"github.com/bitmagnet-io/bitmagnet/internal/database/query"
	"github.com/bitmagnet-io/bitmagnet/internal/database/search"
	"github.com/bitmagnet-io/bitmagnet/internal/model"
	tmdb "github.com/cyruzin/golang-tmdb"
	"strconv"
	"strings"
)

type MovieClient interface {
	SearchMovie(ctx context.Context, p SearchMovieParams) (model.Content, error)
	GetMovieByExternalId(ctx context.Context, source, id string) (model.Content, error)
}

type SearchMovieParams struct {
	Title                string
	Year                 model.Year
	IncludeAdult         bool
	LevenshteinThreshold uint
}

func (c *client) SearchMovie(ctx context.Context, p SearchMovieParams) (movie model.Content, err error) {
	if localResult, localErr := c.searchMovieLocal(ctx, p); localErr == nil {
		return localResult, nil
	} else if !errors.Is(localErr, classifier.ErrNoMatch) {
		err = localErr
		return
	}
	return c.searchMovieTmdb(ctx, p)
}

func (c *client) searchMovieLocal(ctx context.Context, p SearchMovieParams) (movie model.Content, err error) {
	options := []query.Option{
		query.Where(search.ContentTypeCriteria(model.ContentTypeMovie, model.ContentTypeXxx)),
		query.QueryString(fmt.Sprintf("\"%s\"", p.Title)),
		query.OrderByQueryStringRank(),
		query.Limit(5),
		search.ContentDefaultPreload(),
		search.ContentDefaultHydrate(),
	}
	if !p.Year.IsNil() {
		options = append(options, query.Where(search.ContentReleaseDateCriteria(model.NewDateRangeFromYear(p.Year))))
	}
	result, searchErr := c.s.Content(
		ctx,
		options...,
	)
	if searchErr != nil {
		err = searchErr
		return
	}
	for _, item := range result.Items {
		candidates := []string{item.Title}
		if item.OriginalTitle.Valid {
			candidates = append(candidates, item.OriginalTitle.String)
		}
		if levenshteinCheck(p.Title, candidates, p.LevenshteinThreshold) {
			return item.Content, nil
		}
	}
	err = classifier.ErrNoMatch
	return
}

func (c *client) searchMovieTmdb(ctx context.Context, p SearchMovieParams) (model.Content, error) {
	urlOptions := make(map[string]string)
	if !p.Year.IsNil() {
		urlOptions["year"] = strconv.Itoa(int(p.Year))
	}
	if p.IncludeAdult {
		urlOptions["include_adult"] = "true"
	}
	searchResult, searchErr := c.c.GetSearchMovies(
		p.Title,
		urlOptions,
	)
	if searchErr != nil {
		return model.Content{}, searchErr
	}
	for _, item := range searchResult.Results {
		if levenshteinCheck(p.Title, []string{item.Title, item.OriginalTitle}, p.LevenshteinThreshold) {
			return c.GetMovieByExternalId(ctx, SourceTmdb, strconv.Itoa(int(item.ID)))
		}
	}
	return model.Content{}, classifier.ErrNoMatch
}

func (c *client) GetMovieByExternalId(ctx context.Context, source, id string) (model.Content, error) {
	options := []query.Option{
		query.Where(
			search.ContentTypeCriteria(model.ContentTypeMovie, model.ContentTypeXxx),
		),
		search.ContentDefaultPreload(),
		search.ContentDefaultHydrate(),
		query.Limit(1),
	}
	if source == SourceTmdb {
		canonicalResult, canonicalErr := c.s.Content(ctx,
			append(options, query.Where(
				search.ContentCanonicalIdentifierCriteria(model.ContentRef{
					Source: source,
					ID:     id,
				}),
			))...,
		)
		if canonicalErr != nil {
			return model.Content{}, canonicalErr
		}
		if len(canonicalResult.Items) > 0 {
			return canonicalResult.Items[0].Content, nil
		}
	} else {
		alternativeResult, alternativeErr := c.s.Content(ctx,
			append(options, query.Where(
				search.ContentAlternativeIdentifierCriteria(model.ContentRef{
					Source: source,
					ID:     id,
				}),
			))...,
		)
		if alternativeErr != nil {
			return model.Content{}, alternativeErr
		}
		if len(alternativeResult.Items) > 0 {
			return alternativeResult.Items[0].Content, nil
		}
	}
	if source == SourceTmdb {
		intId, idErr := strconv.Atoi(id)
		if idErr != nil {
			return model.Content{}, idErr
		}
		return c.getMovieByTmbdId(ctx, intId)
	}
	externalSource, externalId, externalSourceErr := getExternalSource(source, id)
	if externalSourceErr != nil {
		return model.Content{}, externalSourceErr
	}
	byIdResult, byIdErr := c.c.GetFindByID(externalId, map[string]string{
		"external_source": externalSource,
	})
	if byIdErr != nil {
		return model.Content{}, byIdErr
	}
	if len(byIdResult.MovieResults) == 0 {
		return model.Content{}, classifier.ErrNoMatch
	}
	return c.getMovieByTmbdId(ctx, int(byIdResult.MovieResults[0].ID))
}

const SourceImdb = "imdb"
const SourceTvdb = "tvdb"

func getExternalSource(source string, id string) (externalSource string, externalId string, err error) {
	switch source {
	case SourceImdb:
		externalSource = "imdb_id"
		externalId = id
	case SourceTvdb:
		externalSource = "tvdb_id"
		externalId = id
	default:
		err = ErrUnknownSource
	}
	return
}

func (c *client) getMovieByTmbdId(ctx context.Context, id int) (movie model.Content, err error) {
	d, getDetailsErr := c.c.GetMovieDetails(id, map[string]string{})
	if getDetailsErr != nil {
		// a hacky workaround for TMDB returning 404 for some (correct) movie IDs
		// e.g. there's some issue with tt15168124 which points to 878564 when the correct ID is 888491
		// (haven't added for TV shows as I haven't encountered any examples)
		if strings.HasPrefix(getDetailsErr.Error(), "code: 34") {
			getDetailsErr = classifier.ErrNoMatch
		}
		err = getDetailsErr
		return
	}
	return MovieDetailsToMovieModel(*d)
}

func MovieDetailsToMovieModel(details tmdb.MovieDetails) (movie model.Content, err error) {
	releaseDate := model.Date{}
	if details.ReleaseDate != "" {
		parsedDate, parseDateErr := model.NewDateFromIsoString(details.ReleaseDate)
		if parseDateErr != nil {
			err = parseDateErr
			return
		}
		releaseDate = parsedDate
	}
	var collections []model.ContentCollection
	if details.BelongsToCollection.ID != 0 {
		collections = append(collections, model.ContentCollection{
			Type:   "franchise",
			Source: SourceTmdb,
			ID:     strconv.Itoa(int(details.BelongsToCollection.ID)),
			Name:   details.BelongsToCollection.Name,
		})
	}
	for _, genre := range details.Genres {
		collections = append(collections, model.ContentCollection{
			Type:   "genre",
			Source: SourceTmdb,
			ID:     strconv.Itoa(int(genre.ID)),
			Name:   genre.Name,
		})
	}
	var attributes []model.ContentAttribute
	if details.IMDbID != "" {
		attributes = append(attributes, model.ContentAttribute{
			Source: "imdb",
			Key:    "id",
			Value:  details.IMDbID,
		})
	}
	if details.PosterPath != "" {
		attributes = append(attributes, model.ContentAttribute{
			Source: "tmdb",
			Key:    "poster_path",
			Value:  details.PosterPath,
		})
	}
	if details.BackdropPath != "" {
		attributes = append(attributes, model.ContentAttribute{
			Source: "tmdb",
			Key:    "backdrop_path",
			Value:  details.BackdropPath,
		})
	}
	releaseYear := releaseDate.Year

	typeVideo := model.ContentTypeMovie

	if details.Adult {
		typeVideo = model.ContentTypeXxx
	}

	return model.Content{
		Type:             typeVideo,
		Source:           SourceTmdb,
		ID:               strconv.Itoa(int(details.ID)),
		Title:            details.Title,
		ReleaseDate:      releaseDate,
		ReleaseYear:      releaseYear,
		Adult:            model.NewNullBool(details.Adult),
		OriginalLanguage: model.ParseLanguage(details.OriginalLanguage),
		OriginalTitle:    model.NewNullString(details.OriginalTitle),
		Overview: model.NullString{
			String: details.Overview,
			Valid:  details.Overview != "",
		},
		Runtime: model.NullUint16{
			Uint16: uint16(details.Runtime),
			Valid:  details.Runtime > 0,
		},
		Popularity:  model.NewNullFloat32(details.Popularity),
		VoteAverage: model.NewNullFloat32(details.VoteAverage),
		VoteCount:   model.NewNullUint(uint(details.VoteCount)),
		Collections: collections,
		Attributes:  attributes,
	}, nil
}
